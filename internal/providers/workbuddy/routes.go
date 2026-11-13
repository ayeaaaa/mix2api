package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	adminMaxBodyBytes = 8 << 20
	healthCacheTTL    = 5 * time.Second
)

type wbHealthCacheEntry struct {
	at    time.Time
	value providers.Health
}

// wbAdminError is the provider-local panel error envelope.
type wbAdminError struct {
	Status  int
	Type    string
	Message string
}

func (e *wbAdminError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func wbBadRequest(message string) *wbAdminError {
	return &wbAdminError{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: message}
}

func wbServerError(message string) *wbAdminError {
	return &wbAdminError{Status: http.StatusInternalServerError, Type: "server_error", Message: message}
}

func wbUnavailable(message string) *wbAdminError {
	return &wbAdminError{Status: http.StatusServiceUnavailable, Type: "server_error", Message: message}
}

func wbAdminErrorFrom(err error) *wbAdminError {
	if err == nil {
		return wbServerError("workbuddy: request failed")
	}
	var adminErr *wbAdminError
	if errors.As(err, &adminErr) && adminErr != nil {
		return adminErr
	}
	switch {
	case errors.Is(err, ErrAccountNotFound):
		return &wbAdminError{Status: http.StatusNotFound, Type: "invalid_request_error", Message: "account not found"}
	case errors.Is(err, ErrRealmMismatch):
		return wbBadRequest("account belongs to a different realm")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &wbAdminError{Status: http.StatusBadGateway, Type: "server_error", Message: "workbuddy: request canceled"}
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		status := http.StatusBadGateway
		if apiErr.Kind == KindInvalid {
			status = http.StatusBadRequest
		}
		message := strings.TrimSpace(apiErr.SafeMessage())
		if message == "" {
			message = "workbuddy: request failed"
		}
		return &wbAdminError{Status: status, Type: "invalid_request_error", Message: message}
	}
	var httpErr providers.HTTPError
	if errors.As(err, &httpErr) {
		status := http.StatusBadGateway
		if httpErr.HTTPStatus() == http.StatusBadRequest {
			status = http.StatusBadRequest
		}
		message := strings.TrimSpace(httpErr.SafeMessage())
		if message == "" {
			message = "workbuddy: request failed"
		}
		return &wbAdminError{Status: status, Type: "invalid_request_error", Message: message}
	}
	// Local filesystem/programming errors may contain absolute paths. Keep the
	// raw value server-side and return a generic message on the wire.
	log.Printf("workbuddy: admin internal error: %v", err)
	return &wbAdminError{Status: http.StatusBadGateway, Type: "invalid_request_error", Message: "workbuddy: request failed"}
}

func writeWBAdminJSON(w http.ResponseWriter, status int, value any) {
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		body := []byte(`{"error":{"message":"response encoding failed","type":"server_error","code":500}}`)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(body)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw.Bytes()))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err == nil {
		value = maskPanelNames(generic)
	}
	var buffer bytes.Buffer
	encoder = json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		body := []byte(`{"error":{"message":"response encoding failed","type":"server_error","code":500}}`)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(body)
		return
	}
	body := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

var panelPhonePattern = regexp.MustCompile(`(1[3-9]\d)[ \t\-.·]?(\d{4})[ \t\-.·]?(\d{4})`)

var panelIdentityKeys = map[string]bool{
	"uid": true, "account": true, "accountId": true, "id": true,
	"file": true, "path": true, "machineId": true, "sessionId": true,
	"key": true, "token": true, "accessToken": true, "refreshToken": true,
}

func panelIdentifierByte(value byte) bool {
	return value == '_' || (value >= '0' && value <= '9') ||
		(value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z')
}

func maskPanelPhoneText(text string) string {
	matches := panelPhonePattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var builder strings.Builder
	last := 0
	for _, match := range matches {
		start, end := match[0], match[1]
		if (start > 0 && panelIdentifierByte(text[start-1])) ||
			(end < len(text) && panelIdentifierByte(text[end])) {
			continue
		}
		builder.WriteString(text[last:start])
		builder.WriteString(text[match[2]:match[3]])
		builder.WriteString("****")
		builder.WriteString(text[match[6]:match[7]])
		last = end
	}
	builder.WriteString(text[last:])
	return builder.String()
}

// maskPanelNames mirrors v1's panel-side nickname masking: phone numbers in
// nickname fields and in preformatted strings are masked (138****5678) while
// identity keys are never rewritten. Only a string value directly under an
// identity key is skipped; nested objects under that key are still projected.
// Export documents use the same response writer, matching v1's export
// redaction.
func maskPanelNames(value any) any {
	switch typed := value.(type) {
	case string:
		return maskPanelPhoneText(typed)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if panelIdentityKeys[key] {
				if _, ok := item.(string); ok {
					out[key] = item
					continue
				}
			}
			out[key] = maskPanelNames(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = maskPanelNames(item)
		}
		return out
	default:
		return value
	}
}

func writeWBAdminError(w http.ResponseWriter, failure *wbAdminError) {
	if failure == nil {
		failure = wbServerError("workbuddy: request failed")
	}
	status := failure.Status
	if status == 0 {
		status = http.StatusBadGateway
	}
	errType := failure.Type
	if errType == "" {
		errType = "invalid_request_error"
	}
	message := failure.Message
	if message == "" {
		message = "workbuddy: request failed"
	}
	writeWBAdminJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    status,
		},
	})
}

func readWBAdminValue(w http.ResponseWriter, r *http.Request) (any, *wbAdminError) {
	if r == nil || r.Body == nil {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]any{}, nil
		}
		return nil, wbBadRequest("invalid JSON body")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, wbBadRequest("invalid JSON body")
	}
	return value, nil
}

func readWBAdminPayload(w http.ResponseWriter, r *http.Request) (map[string]any, *wbAdminError) {
	value, failure := readWBAdminValue(w, r)
	if failure != nil {
		return nil, failure
	}
	if value == nil {
		return map[string]any{}, nil
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return nil, wbBadRequest("expected a JSON object")
	}
	return payload, nil
}

func wbAdminText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func wbAdminBool(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case json.Number:
		number, err := typed.Float64()
		return err == nil && number != 0, err == nil
	case float64:
		return typed != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off":
			return false, true
		}
	}
	return false, false
}

func wbAdminInt(value any) (int, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		if err == nil {
			return int(number), true
		}
		float, err := typed.Float64()
		if err == nil {
			return int(float), true
		}
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(typed))
		return number, err == nil
	}
	return 0, false
}

func wbAdminStringList(value any) ([]string, bool) {
	switch typed := value.(type) {
	case nil:
		return []string{}, true
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text := wbAdminText(item)
			if text != "" {
				out = append(out, text)
			}
		}
		return out, true
	case []string:
		return append([]string(nil), typed...), true
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return []string{}, true
		}
		parts := strings.FieldsFunc(text, func(r rune) bool {
			return r == ',' || r == '，' || r == ' ' || r == '\n' || r == '\t'
		})
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out, true
	}
	return nil, false
}

func wbForceFromQuery(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func wbRequestRealm(r *http.Request, payload map[string]any, fallback string) string {
	if payload != nil {
		if realm := strings.ToLower(strings.TrimSpace(wbAdminText(payload["realm"]))); realm != "" {
			return realm
		}
	}
	if r != nil {
		if realm := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("realm"))); realm != "" {
			return realm
		}
	}
	return fallback
}

func wbCloneHealth(value providers.Health) providers.Health {
	if value == nil {
		return nil
	}
	out := make(providers.Health, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

// refreshSettings re-reads this realm's settings and projects them onto the
// live config/pool/proxy slots so panel edits apply without a restart.
func (p *Provider) refreshSettings() Config {
	if p == nil {
		return Config{}
	}
	cfg := p.configForRealm()
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	if pool := p.Pool(); pool != nil {
		pool.SetConfig(cfg)
		if slots := p.loginSlots(); slots != nil {
			pool.ApplyProxySlots(slots)
		}
	}
	return cfg
}

func (p *Provider) invalidateHealth() {
	if p == nil {
		return
	}
	p.healthMu.Lock()
	p.healthCache = wbHealthCacheEntry{}
	p.healthMu.Unlock()
}

// Health returns the v1-compatible health block for this realm.
func (p *Provider) Health(ctx context.Context, force bool) (providers.Health, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := p.refreshSettings()
	now := time.Now()
	p.healthMu.Lock()
	if !force && !p.healthCache.at.IsZero() && now.Sub(p.healthCache.at) < healthCacheTTL {
		value := wbCloneHealth(p.healthCache.value)
		p.healthMu.Unlock()
		return value, nil
	}
	p.healthMu.Unlock()

	stats := PoolStats{}
	if pool := p.Pool(); pool != nil {
		stats = pool.Stats()
	}
	value := providers.Health{
		"ok":         cfg.Enabled,
		"configured": cfg.Enabled,
		"accounts":   stats.Total,
		"healthy":    stats.Healthy,
		"disabled":   stats.Disabled,
		"cooling":    stats.Cooling,
		"provider":   ProviderID,
		"realm":      p.realm,
		"base_url":   p.EndpointFor(productDesktop),
	}
	if meta, ok := p.CatalogMeta(); ok {
		value["catalog_source"] = meta.Source
	}
	p.healthMu.Lock()
	p.healthCache = wbHealthCacheEntry{at: now, value: wbCloneHealth(value)}
	p.healthMu.Unlock()
	return value, nil
}

// Overview returns the v1 admin snapshot for this realm. No credential value
// enters the payload; disable state, credits and timestamps are the panel's
// operational view.
func (p *Provider) Overview(ctx context.Context, force bool) (any, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.refreshSettings()
	health, err := p.Health(ctx, force)
	if err != nil {
		return nil, err
	}
	models, err := p.Models(ctx, force)
	if err != nil {
		return nil, err
	}
	overviewModels := make([]map[string]any, 0, len(models))
	for _, item := range models {
		id := strings.TrimSpace(wbAdminText(item["id"]))
		if id == "" {
			continue
		}
		name := strings.TrimSpace(wbAdminText(item["name"]))
		if name == "" {
			name = id
		}
		overviewModels = append(overviewModels, map[string]any{"id": id, "name": name})
	}
	stats := PoolStats{}
	accounts := []AccountView{}
	if pool := p.Pool(); pool != nil {
		stats = pool.Stats()
		accounts = pool.List()
	}
	return map[string]any{
		"provider":         ProviderID,
		"service":          ProviderID,
		"id":               p.ID(),
		"realm":            p.realm,
		"online":           health["ok"] == true,
		"base_url":         p.EndpointFor(productDesktop),
		"auto_claim_login": false,
		"stats":            stats,
		"accounts":         accounts,
		"health":           health,
		"models":           overviewModels,
	}, nil
}

// RouteNotFoundMessage is consumed by the generic API route adapter.
func (p *Provider) RouteNotFoundMessage() string {
	return "unknown workbuddy endpoint"
}

// Routes returns the legacy unqualified WorkBuddy admin surface on the intl
// provider only. The cn instance deliberately declares none: the panel routes
// are realm-agnostic and delegate realm=cn to the twin, so registering both
// would create duplicate handlers for the same paths.
func (p *Provider) Routes() []providers.Route {
	if p == nil || p.realm != realmIntl {
		return nil
	}
	panel := providers.AuthPanel
	return []providers.Route{
		{Method: http.MethodGet, Path: "/accounts", Auth: panel, Handler: p.handleAccounts},
		{Method: http.MethodGet, Path: "/accounts/export", Auth: panel, Handler: p.handleAccountsExport},
		{Method: http.MethodGet, Path: "/accounts/credits", Auth: panel, Handler: p.handleAccountsCredits},
		{Method: http.MethodPost, Path: "/accounts/credits", Auth: panel, Handler: p.handleAccountsCredits},
		{Method: http.MethodPost, Path: "/accounts/credits/fetch", Auth: panel, Handler: p.handleAccountsCredits},
		{Method: http.MethodGet, Path: "/accounts/credits/detail", Auth: panel, Handler: p.handleAccountsCreditsDetail},
		{Method: http.MethodPost, Path: "/accounts/credits/detail", Auth: panel, Handler: p.handleAccountsCreditsDetail},
		{Method: http.MethodGet, Path: "/accounts/credits/grants", Auth: panel, Handler: p.handleAccountsGrants},
		{Method: http.MethodPost, Path: "/accounts/import", Auth: panel, Handler: p.handleAccountsImport},
		{Method: http.MethodPost, Path: "/accounts/import/desktop", Auth: panel, Handler: p.handleAccountsImportDesktop},
		{Method: http.MethodPost, Path: "/accounts/add-api-key", Auth: panel, Handler: p.handleAccountsAddAPIKey},
		{Method: http.MethodPost, Path: "/accounts/login/start", Auth: panel, Handler: p.handleAccountsLoginStart},
		{Method: http.MethodGet, Path: "/accounts/login/poll", Auth: panel, Handler: p.handleAccountsLoginPoll},
		{Method: http.MethodPost, Path: "/accounts/login/poll", Auth: panel, Handler: p.handleAccountsLoginPoll},
		{Method: http.MethodPost, Path: "/accounts/login/cancel", Auth: panel, Handler: p.handleAccountsLoginCancel},
		{Method: http.MethodPost, Path: "/accounts/checkin", Auth: panel, Handler: p.handleAccountsCheckin},
		{Method: http.MethodPost, Path: "/accounts/daily-chat", Auth: panel, Handler: p.handleAccountsDailyChat},
		{Method: http.MethodPost, Path: "/accounts/daily-chat-web", Auth: panel, Handler: p.handleAccountsDailyChatWeb},
		{Method: http.MethodPost, Path: "/accounts/test", Auth: panel, Handler: p.handleAccountsTest},
		{Method: http.MethodPost, Path: "/accounts/refresh", Auth: panel, Handler: p.handleAccountsRefresh},
		{Method: http.MethodPost, Path: "/accounts/sync-profile", Auth: panel, Handler: p.handleAccountsSyncProfile},
		{Method: http.MethodPost, Path: "/accounts/set", Auth: panel, Handler: p.handleAccountsSet},
		{Method: http.MethodPost, Path: "/accounts/set-all", Auth: panel, Handler: p.handleAccountsSetAll},
		{Method: http.MethodPost, Path: "/accounts/product", Auth: panel, Handler: p.handleAccountsProduct},
		{Method: http.MethodPost, Path: "/accounts/delete", Auth: panel, Handler: p.handleAccountsDelete},
		{Method: http.MethodPost, Path: "/accounts/enable", Auth: panel, Handler: p.handleAccountsEnable},
		{Method: http.MethodPost, Path: "/accounts/disable", Auth: panel, Handler: p.handleAccountsDisable},
		{Method: http.MethodPost, Path: "/accounts/note", Auth: panel, Handler: p.handleAccountsNote},
		{Method: http.MethodPost, Path: "/accounts/tasks", Auth: panel, Handler: p.handleAccountsTasks},
		{Method: http.MethodPost, Path: "/accounts/weight", Auth: panel, Handler: p.handleAccountsWeight},
		{Method: http.MethodPost, Path: "/accounts/group", Auth: panel, Handler: p.handleAccountsGroup},
		{Method: http.MethodPost, Path: "/accounts/clear-cooldown", Auth: panel, Handler: p.handleAccountsClearCooldown},
		{Method: http.MethodGet, Path: "/tasks", Auth: panel, Handler: p.handleTasks},
		{Method: http.MethodPost, Path: "/tasks/run", Auth: panel, Handler: p.handleTasksRun},
		{Method: http.MethodPost, Path: "/tasks/claim", Auth: panel, Handler: p.handleTasksClaim},
		{Method: http.MethodPost, Path: "/tasks/travel", Auth: panel, Handler: p.handleTasksTravel},
		{Method: http.MethodPost, Path: "/tasks/streak-bonus", Auth: panel, Handler: p.handleTasksStreakBonus},
		{Method: http.MethodPost, Path: "/tasks/buddy", Auth: panel, Handler: p.handleTasksBuddy},
		{Method: http.MethodGet, Path: "/activity/history", Auth: panel, Handler: p.handleActivityHistory},
		{Method: http.MethodGet, Path: "/activity/campaigns", Auth: panel, Handler: p.handleActivityCampaigns},
		{Method: http.MethodPost, Path: "/activity/campaigns/run", Auth: panel, Handler: p.handleActivityCampaignsRun},
		{Method: http.MethodPost, Path: "/activity/campaigns/claim", Auth: panel, Handler: p.handleActivityCampaignClaim},
		{Method: http.MethodPost, Path: "/catalog/refresh", Auth: panel, Handler: p.handleCatalogRefresh},
		{Method: http.MethodGet, Path: "/scheduler", Auth: panel, Handler: p.handleScheduler},
		{Method: http.MethodPost, Path: "/scheduler/trigger", Auth: panel, Handler: p.handleSchedulerTrigger},
		{Method: http.MethodPost, Path: "/scheduler/toggle", Auth: panel, Handler: p.handleSchedulerToggle},
		{Method: http.MethodPost, Path: "/settings/restriction/reset", Auth: panel, Handler: p.handleSettingsRestrictionReset},
	}
}

func (p *Provider) providerForRealm(realm string) *Provider {
	if p == nil {
		return nil
	}
	if normalized, ok := normalizeRealm(realm); ok {
		if p.realm == normalized {
			return p
		}
		if peer := p.Peer(); peer != nil && peer.realm == normalized {
			return peer
		}
		return nil
	}
	return nil
}

// routeProviders resolves a panel realm selector. "all" is the WorkBuddy union;
// an empty value uses fallback, and an empty fallback means the union. Invalid
// or unavailable realms are explicit errors rather than silently widening a
// mutation to every account.
func (p *Provider) routeProviders(realm, fallback string) ([]*Provider, *wbAdminError) {
	realm = strings.ToLower(strings.TrimSpace(realm))
	if realm == "" {
		realm = strings.ToLower(strings.TrimSpace(fallback))
	}
	if realm == "" || realm == "all" {
		return p.allProviders(), nil
	}
	normalized, ok := normalizeRealm(realm)
	if !ok {
		return nil, wbBadRequest("realm must be intl, cn or all")
	}
	provider := p.providerForRealm(normalized)
	if provider == nil {
		return nil, p.safeAdminRealmError(normalized)
	}
	return []*Provider{provider}, nil
}

func (p *Provider) allProviders() []*Provider {
	if p == nil {
		return nil
	}
	out := make([]*Provider, 0, 2)
	seen := map[*Provider]bool{}
	add := func(provider *Provider) {
		if provider == nil || seen[provider] {
			return
		}
		seen[provider] = true
		out = append(out, provider)
	}
	add(p)
	add(p.Peer())
	return out
}

func (p *Provider) providerForUID(uid, realm string) (*Provider, *Account) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, nil
	}
	if normalized, ok := normalizeRealm(realm); ok {
		provider := p.providerForRealm(normalized)
		if provider == nil {
			return nil, nil
		}
		return provider, provider.Pool().Get(uid)
	}
	for _, provider := range p.allProviders() {
		if account := provider.Pool().Get(uid); account != nil {
			return provider, account
		}
	}
	return nil, nil
}

// resolveAccountUID keeps an explicit realm selector when possible, then falls
// back to the other realm. v1's single pool never rejected a uid merely because
// the dashboard happened to be showing the other exit.
func (p *Provider) resolveAccountUID(uid, realm string) (*Provider, *Account) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, nil
	}
	realm = strings.ToLower(strings.TrimSpace(realm))
	if _, ok := normalizeRealm(realm); ok {
		if provider := p.providerForRealm(realm); provider != nil {
			if account := provider.Pool().Get(uid); account != nil {
				return provider, account
			}
		}
	}
	return p.providerForUID(uid, "")
}

func (p *Provider) safeAdminRealmError(realm string) *wbAdminError {
	return wbUnavailable(fmt.Sprintf("workbuddy: %s realm is not available", strings.ToLower(strings.TrimSpace(realm))))
}

func (p *Provider) handleAccounts(w http.ResponseWriter, r *http.Request) {
	realm := wbRequestRealm(r, nil, "")
	if realm == "" {
		// v1's GET /accounts uses the persisted active exit when the caller did
		// not name one. Only an explicit "all" is the cross-realm union.
		realm = p.activeRealm()
	}
	providersList := []*Provider{}
	if realm == "all" || realm == "" {
		providersList = p.allProviders()
	} else {
		provider := p.providerForRealm(realm)
		if provider == nil {
			writeWBAdminError(w, p.safeAdminRealmError(realm))
			return
		}
		providersList = []*Provider{provider}
	}
	accounts := make([]AccountView, 0)
	storage := ""
	usable := 0
	for _, provider := range providersList {
		provider.refreshSettings()
		pool := provider.Pool()
		if pool == nil {
			continue
		}
		accounts = append(accounts, pool.List()...)
		if storage == "" {
			storage = provider.AccountsDir()
		}
		usable += pool.CountReady("")
	}
	sort.SliceStable(accounts, func(i, j int) bool { return accounts[i].UID < accounts[j].UID })
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"accounts": accounts,
		"storage":  storage,
		"usable":   usable,
	})
}

// allAccountViews returns the whole WorkBuddy pool (both realms) in the
// deterministic UID order v1's account_views() used for panel responses.
func (p *Provider) allAccountViews() []AccountView {
	accounts := make([]AccountView, 0)
	for _, provider := range p.allProviders() {
		if pool := provider.Pool(); pool != nil {
			accounts = append(accounts, pool.List()...)
		}
	}
	sort.SliceStable(accounts, func(i, j int) bool { return accounts[i].UID < accounts[j].UID })
	return accounts
}

func (p *Provider) handleAccountsExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	realm := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("realm")))
	if realm != "" && realm != realmIntl && realm != realmCN && realm != "all" {
		writeWBAdminError(w, wbBadRequest("realm must be intl or cn"))
		return
	}
	providersList := p.allProviders()
	if realm == realmIntl || realm == realmCN {
		provider := p.providerForRealm(realm)
		if provider == nil {
			writeWBAdminError(w, p.safeAdminRealmError(realm))
			return
		}
		providersList = []*Provider{provider}
	}
	wanted := map[string]bool{}
	for _, raw := range r.URL.Query()["uid"] {
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				wanted[part] = true
			}
		}
	}
	known := map[string]bool{}
	for _, provider := range providersList {
		for _, account := range provider.Pool().Accounts() {
			known[account.UID()] = true
		}
	}
	missing := make([]string, 0)
	for uid := range wanted {
		if !known[uid] {
			missing = append(missing, uid)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		writeWBAdminError(w, &wbAdminError{
			Status: http.StatusNotFound, Type: "invalid_request_error",
			Message: "no such account: " + strings.Join(missing[:wbMinInt(5, len(missing))], ", "),
		})
		return
	}
	includeSecrets := true
	// v1 parity: export carries credentials by default. The route is
	// panel-authenticated; callers sharing a document must pass secrets=0.
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("secrets"))) {
	case "0", "false", "no":
		includeSecrets = false
	}
	rows := make([]map[string]any, 0)
	for _, provider := range providersList {
		for _, account := range provider.Pool().Accounts() {
			uid := account.UID()
			if len(wanted) > 0 && !wanted[uid] {
				continue
			}
			account.mu.RLock()
			raw, err := json.Marshal(account.Auth)
			account.mu.RUnlock()
			if err != nil {
				writeWBAdminError(w, wbAdminErrorFrom(err))
				return
			}
			row := map[string]any{}
			if err := json.Unmarshal(raw, &row); err != nil {
				writeWBAdminError(w, wbAdminErrorFrom(err))
				return
			}
			delete(row, "path")
			if !includeSecrets {
				stripExportSecretKeys(row)
			}
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return wbAdminText(rows[i]["uid"]) < wbAdminText(rows[j]["uid"])
	})
	document := map[string]any{
		"format":     "workbuddy-accounts",
		"version":    1,
		"exportedAt": time.Now().Format("2006-01-02 15:04:05"),
		"count":      len(rows),
		"accounts":   rows,
	}
	if download, _ := wbAdminBool(r.URL.Query().Get("download")); download {
		name := "workbuddy-accounts-" + time.Now().Format("20060102-150405") + ".json"
		if len(wanted) == 1 {
			for uid := range wanted {
				name = "workbuddy-accounts-" + firstNonEmpty(uid[:wbMinInt(8, len(uid))], "account") + "-" + time.Now().Format("20060102-150405") + ".json"
			}
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	writeWBAdminJSON(w, http.StatusOK, document)
}

func (p *Provider) handleAccountsCredits(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{}
	if r.Method == http.MethodPost {
		value, failure := readWBAdminPayload(w, r)
		if failure != nil {
			writeWBAdminError(w, failure)
			return
		}
		payload = value
	}
	targets := CreditsRefreshTargets{
		UID:   wbAdminText(payload["uid"]),
		Group: wbAdminText(payload["group"]),
	}
	if raw, present := payload["uids"]; present {
		values, ok := wbAdminStringList(raw)
		if !ok {
			writeWBAdminError(w, wbBadRequest("uids must be a list"))
			return
		}
		targets.UIDs = values
	}
	selector := wbRequestRealm(r, payload, "")
	if targets.UID != "" {
		if selector != "" && selector != "all" {
			if _, ok := normalizeRealm(selector); !ok {
				writeWBAdminError(w, wbBadRequest("realm must be intl, cn or all"))
				return
			}
		}
		if provider, account := p.resolveAccountUID(targets.UID, selector); account != nil {
			provider.refreshSettings()
			local := targets
			local.Realm = provider.realm
			result, err := provider.refreshCreditsRoute(r.Context(), local)
			if err != nil {
				writeWBAdminError(w, wbAdminErrorFrom(err))
				return
			}
			provider.invalidateHealth()
			p.writeCreditsResponse(w, r, result.Results, result.Busy)
			return
		}
	}
	realm := selector
	if realm == "" || realm == "all" {
		// v1's unqualified sweep covered the whole pool. The two realms are
		// separate provider instances here, so an omitted realm means both;
		// each provider still resolves uid/uids/group against its own accounts.
		results := make([]CreditRefreshRow, 0)
		busy := false
		paced := false
		for _, provider := range p.allProviders() {
			provider.refreshSettings()
			local := targets
			local.Realm = provider.realm
			if len(provider.creditTargetAccounts(provider.Pool(), local)) > 0 {
				if paced {
					provider.taskSleepFor(provider.taskGapDelay())
				}
				paced = true
			}
			value, err := provider.refreshCreditsRoute(r.Context(), local)
			if err != nil {
				writeWBAdminError(w, wbAdminErrorFrom(err))
				return
			}
			busy = busy || value.Busy
			results = append(results, value.Results...)
		}
		p.writeCreditsResponse(w, r, results, busy)
		return
	}
	provider := p.providerForRealm(realm)
	if provider == nil {
		writeWBAdminError(w, p.safeAdminRealmError(realm))
		return
	}
	provider.refreshSettings()
	targets.Realm = provider.realm
	result, err := provider.refreshCreditsRoute(r.Context(), targets)
	if err != nil {
		writeWBAdminError(w, wbAdminErrorFrom(err))
		return
	}
	provider.invalidateHealth()
	p.writeCreditsResponse(w, r, result.Results, result.Busy)
}

// writeCreditsResponse keeps v1's envelope: the accounts array is always the
// whole WorkBuddy pool, while results only contain the requested targets. GET
// always carries busy (v1's snapshot endpoint); POST carries it only while a
// sweep is already running, so the normal response has no busy key at all.
func (p *Provider) writeCreditsResponse(w http.ResponseWriter, r *http.Request, results []CreditRefreshRow, busy bool) {
	payload := map[string]any{"results": results, "accounts": p.allAccountViews()}
	if (r != nil && r.Method == http.MethodGet) || busy {
		payload["busy"] = busy
	}
	writeWBAdminJSON(w, http.StatusOK, payload)
}

func (p *Provider) refreshCreditsRoute(ctx context.Context, targets CreditsRefreshTargets) (CreditsRefreshResult, error) {
	pool := p.Pool()
	if pool == nil {
		return CreditsRefreshResult{Results: []CreditRefreshRow{}}, errors.New("workbuddy: account pool is nil")
	}
	targetAccounts := p.creditTargetAccounts(pool, targets)
	if len(targetAccounts) > 1 {
		if !p.creditsSweepMu.TryLock() {
			return CreditsRefreshResult{Results: []CreditRefreshRow{}, Accounts: pool.List(), Busy: true}, nil
		}
		defer p.creditsSweepMu.Unlock()
	}
	return p.RefreshCreditsFor(ctx, targets)
}

func (p *Provider) handleAccountsCreditsDetail(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{}
	if r.Method == http.MethodPost {
		value, failure := readWBAdminPayload(w, r)
		if failure != nil {
			writeWBAdminError(w, failure)
			return
		}
		payload = value
	}
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	if uid == "" {
		uid = wbAdminText(payload["uid"])
	}
	realm := wbRequestRealm(r, payload, "")
	var provider *Provider
	var account *Account
	if realm == "" || realm == "all" {
		if uid != "" {
			// v1 looked a uid up in the single pool; v2 resolves it across both
			// realm providers so an unqualified detail request cannot miss a
			// CN account just because the legacy route is owned by intl.
			provider, account = p.providerForUID(uid, "")
		} else {
			for _, candidate := range p.allProviders() {
				if pool := candidate.Pool(); pool != nil && len(pool.Accounts()) > 0 {
					provider = candidate
					account = pool.Accounts()[0]
					break
				}
			}
		}
	} else {
		provider = p.providerForRealm(realm)
		if provider == nil {
			writeWBAdminError(w, p.safeAdminRealmError(realm))
			return
		}
		account = provider.Pool().Get(uid)
		if account == nil && uid != "" {
			provider, account = p.resolveAccountUID(uid, realm)
		}
		if account == nil && uid == "" && len(provider.Pool().Accounts()) > 0 {
			account = provider.Pool().Accounts()[0]
		}
	}
	if provider == nil || account == nil {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "未找到指定账号"})
		return
	}
	provider.refreshSettings()
	refresh := false
	if r.Method == http.MethodPost {
		refresh = true
		if raw, present := payload["refresh"]; present {
			refresh, _ = wbAdminBool(raw)
		}
	} else if raw := r.URL.Query().Get("refresh"); raw != "" {
		refresh, _ = wbAdminBool(raw)
	}
	snapshot := creditSnapshotOf(account)
	if refresh || snapshot == nil || len(snapshot.Packages) == 0 {
		row := provider.refreshCreditAccount(r.Context(), account)
		snapshot = row.Credits
		if !row.OK {
			writeWBAdminJSON(w, http.StatusOK, map[string]any{
				"ok": false, "uid": account.UID(), "nickname": accountNicknameOf(account),
				"realm": provider.realm, "credits": snapshot,
				"expiring": account.ExpirySummary(),
				"error":    firstNonEmpty(row.Error, "获取积分明细失败"),
			})
			return
		}
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": account.UID(), "nickname": accountNicknameOf(account),
		"realm": provider.realm, "credits": snapshot,
		"expiring": account.ExpirySummary(),
	})
}

func (p *Provider) handleAccountsGrants(w http.ResponseWriter, r *http.Request) {
	providersList, failure := p.routeProviders(wbRequestRealm(r, nil, ""), "")
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	if len(providersList) == 1 {
		result, err := providersList[0].Grants(r.Context())
		if err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		writeWBAdminJSON(w, http.StatusOK, result)
		return
	}
	merged := GrantsResult{OK: true, Rows: []GrantRow{}}
	fetched := 0.0
	fetchedISO := ""
	for _, provider := range providersList {
		result, err := provider.Grants(r.Context())
		if err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		merged.GeneratedAt = result.GeneratedAt
		merged.GeneratedISO = result.GeneratedISO
		merged.Rows = append(merged.Rows, result.Rows...)
		if result.FetchedAt != nil && *result.FetchedAt > fetched {
			fetched = *result.FetchedAt
			if result.FetchedISO != nil {
				fetchedISO = *result.FetchedISO
			}
		}
	}
	if fetched > 0 {
		merged.FetchedAt = &fetched
		merged.FetchedISO = &fetchedISO
	}
	merged.Summary = GrantSummary{Count: len(merged.Rows)}
	for _, row := range merged.Rows {
		merged.Summary.Size += row.Size
		merged.Summary.Remain += row.Remain
		merged.Summary.Used += row.Used
	}
	merged.Summary.Size = roundCredit(merged.Summary.Size)
	merged.Summary.Remain = roundCredit(merged.Summary.Remain)
	merged.Summary.Used = roundCredit(merged.Summary.Used)
	accounts := map[string]bool{}
	for _, row := range merged.Rows {
		accounts[row.UID] = true
	}
	merged.Summary.Accounts = len(accounts)
	writeWBAdminJSON(w, http.StatusOK, merged)
}

func (p *Provider) handleAccountsImport(w http.ResponseWriter, r *http.Request) {
	value, failure := readWBAdminValue(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	var document any = value
	options := map[string]any{}
	if object, ok := value.(map[string]any); ok {
		options = object
		if nested, present := object["data"]; present {
			document = nested
		}
	}
	data, err := json.Marshal(document)
	if err != nil {
		writeWBAdminError(w, wbBadRequest("the document must be JSON"))
		return
	}
	rows, err := decodeImportRows(data)
	if err != nil {
		writeWBAdminError(w, wbBadRequest("cannot read the document: "+err.Error()))
		return
	}
	forcedRealm := strings.ToLower(strings.TrimSpace(wbAdminText(options["realm"])))
	if forcedRealm != "" && forcedRealm != realmIntl && forcedRealm != realmCN {
		writeWBAdminError(w, wbBadRequest("realm must be intl or cn"))
		return
	}
	forcedGroup := strings.TrimSpace(wbAdminText(options["group"]))
	if len(forcedGroup) > maxGroupRunes {
		forcedGroup = truncateText(forcedGroup, maxGroupRunes)
	}
	if forcedGroup != "" {
		for _, row := range rows {
			row["group"] = forcedGroup
		}
	}
	dryRun, _ := wbAdminBool(options["dryRun"])
	overwrite, _ := wbAdminBool(options["overwrite"])

	if dryRun {
		preview := previewImportRowsForDir(p.AccountsDir(), rows, forcedRealm, overwrite)
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"dryRun":   true,
			"count":    len(rows),
			"result":   preview,
			"accounts": p.importAccountsViews(forcedRealm),
		})
		return
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		writeWBAdminError(w, wbServerError("workbuddy: import failed"))
		return
	}
	result, err := ImportAccounts(p.AccountsDir(), payload, forcedRealm, overwrite)
	if err != nil {
		writeWBAdminError(w, wbBadRequest(err.Error()))
		return
	}
	for _, provider := range p.allProviders() {
		provider.refreshSettings()
		for _, uid := range append(append([]string(nil), result.Added...), result.Updated...) {
			provider.syncAccountFromDisk(uid)
			provider.bestEffortInitialCheckin(r.Context(), uid, taskTriggerAccountImport)
		}
		provider.invalidateHealth()
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"count":    len(rows),
		"result":   result,
		"accounts": p.importAccountsViews(forcedRealm),
	})
}

func stripExportSecretKeys(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{
			"accessToken", "refreshToken", "access_token", "refresh_token",
			"proxy", "deviceToken", "device_token", "device-token",
			"authorization", "cookie", "set-cookie", "password", "secret",
		} {
			delete(typed, key)
		}
		for _, item := range typed {
			stripExportSecretKeys(item)
		}
	case []any:
		for _, item := range typed {
			stripExportSecretKeys(item)
		}
	}
}

// previewImportRowsForDir shares the exact existing-account view with the real
// import; it exists so dryRun and import cannot disagree.
func previewImportRowsForDir(dir string, rows []map[string]any, forcedRealm string, overwrite bool) ImportResult {
	result := ImportResult{Added: []string{}, Updated: []string{}, Skipped: []ImportIssue{}, Invalid: []ImportIssue{}}
	known := map[string]bool{}
	if accounts, err := LoadAccounts(dir); err == nil {
		for _, account := range accounts {
			known[account.UID] = true
		}
	}
	seen := map[string]bool{}
	for index, row := range rows {
		auth, _, err := normalizeImportRow(row, forcedRealm)
		if err != nil {
			result.Invalid = append(result.Invalid, ImportIssue{Index: index + 1, Reason: err.Error()})
			continue
		}
		uid := auth.UID
		if seen[uid] {
			result.Skipped = append(result.Skipped, ImportIssue{UID: uid, Reason: "duplicate inside the document"})
			continue
		}
		seen[uid] = true
		if known[uid] && !overwrite {
			result.Skipped = append(result.Skipped, ImportIssue{UID: uid, Reason: "already exists"})
			continue
		}
		if known[uid] {
			result.Updated = append(result.Updated, uid)
		} else {
			result.Added = append(result.Added, uid)
		}
	}
	return result
}

func (p *Provider) importAccountsViews(realm string) []AccountView {
	providersList := p.allProviders()
	if realm == realmIntl || realm == realmCN {
		if provider := p.providerForRealm(realm); provider != nil {
			providersList = []*Provider{provider}
		}
	}
	out := make([]AccountView, 0)
	for _, provider := range providersList {
		if pool := provider.Pool(); pool != nil {
			out = append(out, pool.List()...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

func (p *Provider) syncAccountFromDisk(uid string) {
	uid = strings.TrimSpace(uid)
	if p == nil || uid == "" {
		return
	}
	accounts, err := LoadAccounts(p.AccountsDir())
	if err != nil {
		return
	}
	for _, auth := range accounts {
		if auth.UID != uid || auth.Realm != p.realm {
			continue
		}
		_, _ = p.Pool().Add(auth)
		return
	}
}

// bestEffortInitialCheckin mirrors v1's account_add/account_import hook for a
// newly saved CN personal account. Enterprise and static API-key accounts have
// no check-in programme, and a failed sign-in must never fail the add/import
// request that already succeeded.
func (p *Provider) bestEffortInitialCheckin(ctx context.Context, uid, trigger string) {
	if p == nil {
		return
	}
	account := p.Pool().Get(uid)
	if account == nil {
		return
	}
	auth := taskAuthSnapshot(account)
	if auth.Realm != realmCN || taskIsAPIKey(auth) || taskEnterpriseID(auth) != "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// The add/import response must not hang on a slow billing endpoint, and a
	// client disconnect must not cancel the initial sign-in halfway through.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tasksDefaultTimeout+5*time.Second)
	defer cancel()
	result, err := p.runCheckin(ctx, account, trigger)
	if err != nil {
		return
	}
	p.recordActivity(result.Activity)
}

func (p *Provider) handleAccountsAddAPIKey(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	key := strings.TrimSpace(firstNonEmpty(wbAdminText(payload["key"]), wbAdminText(payload["accessToken"])))
	if key == "" {
		writeWBAdminError(w, wbBadRequest("key is required"))
		return
	}
	if !IsAPIKeyToken(key) {
		writeWBAdminError(w, wbBadRequest("this is not a static API key: expected the ck_/pt_ prefix issued by the CodeBuddy API console"))
		return
	}
	provider := p.providerForRealm(realmCN)
	if provider == nil {
		writeWBAdminError(w, p.safeAdminRealmError(realmCN))
		return
	}
	provider.refreshSettings()
	auth := Auth{
		UID:             wbAdminText(payload["uid"]),
		Nickname:        wbAdminText(payload["nickname"]),
		Realm:           realmCN,
		Domain:          domainForRealm(realmCN),
		AccessToken:     key,
		APIKey:          true,
		Enabled:         true,
		EnabledSet:      true,
		TasksEnabled:    false,
		TasksEnabledSet: true,
		Source:          sourceAPIKey,
	}
	normalized, err := normalizeAuth(auth)
	if err != nil || strings.TrimSpace(normalized.UID) == "" {
		writeWBAdminError(w, wbBadRequest("cannot derive a uid from this key; pass uid explicitly"))
		return
	}
	maxConcurrent := provider.Pool().accountMaxConcurrentLocked(provider.Config())
	probeAccount := provider.Pool().newAccount(normalized, maxConcurrent)
	snapshot, probeErr := provider.fetchEnterpriseCredits(r.Context(), newCreditsSession(provider, probeAccount))
	if probeErr != nil {
		writeWBAdminError(w, wbBadRequest("the key did not pass the balance probe, so it was not saved: "+wbSafeErrorText(probeErr)))
		return
	}
	existed := provider.Pool().Get(normalized.UID) != nil
	account, err := provider.Pool().Add(normalized)
	if err != nil {
		writeWBAdminError(w, wbAdminErrorFrom(err))
		return
	}
	if snapshot != nil {
		_ = provider.Pool().SetCredits(normalized.UID, snapshot)
	}
	provider.bestEffortInitialCheckin(r.Context(), normalized.UID, taskTriggerAccountAdd)
	provider.invalidateHealth()
	view := account.View()
	probe := map[string]any{"ok": true}
	if snapshot != nil {
		probe["used"] = snapshot.Used
		probe["limit"] = snapshot.Size
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"uid":      view.UID,
		"nickname": firstNonEmpty(view.Nickname, shortUID(view.UID)),
		"updated":  existed,
		"probe":    probe,
		"account":  view,
	})
}

func wbSafeErrorText(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr != nil && strings.TrimSpace(apiErr.SafeMessage()) != "" {
		return apiErr.SafeMessage()
	}
	return "request failed"
}

func (p *Provider) handleAccountsImportDesktop(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	if forget, _ := wbAdminBool(payload["forgetKey"]); forget {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"atrest": map[string]any{"available": false},
		})
		return
	}
	if keyText := strings.TrimSpace(wbAdminText(payload["keyText"])); keyText != "" {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"msg":    "encrypted desktop credentials are not supported in v2; use OAuth instead",
			"atrest": map[string]any{"available": false},
		})
		return
	}
	if recover, _ := wbAdminBool(payload["recoverKey"]); recover {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"msg":    "desktop key recovery is not available in v2; use OAuth instead",
			"atrest": map[string]any{"available": false},
		})
		return
	}
	candidates := safeDesktopCandidates(p.desktopDirs)
	targetPath := strings.TrimSpace(wbAdminText(payload["path"]))
	if targetPath != "" {
		resolvedPath, hint, found := resolveKnownDesktopCredential(p.desktopDirs, targetPath)
		if !found {
			writeWBAdminError(w, wbBadRequest("desktop credential path is not a known client credential"))
			return
		}
		auth, err := ImportDesktopCredential(p.AccountsDir(), resolvedPath, hint)
		if err != nil {
			writeWBAdminError(w, wbBadRequest("import failed: "+err.Error()))
			return
		}
		for _, provider := range p.allProviders() {
			provider.refreshSettings()
			provider.syncAccountFromDisk(auth.UID)
			provider.bestEffortInitialCheckin(r.Context(), auth.UID, taskTriggerAccountImport)
			provider.invalidateHealth()
		}
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"imported": p.accountViewsForUID(auth.UID),
			"accounts": p.importAccountsViews(""),
		})
		return
	}
	imported := make([]AccountView, 0)
	if all, _ := wbAdminBool(payload["all"]); all {
		realm := strings.ToLower(strings.TrimSpace(wbAdminText(payload["realm"])))
		for _, candidate := range candidates {
			// Filter the detected realm before ImportDesktopCredential writes
			// anything: v1's bulk import never persisted a credential just to
			// discard it because the caller selected the other realm.
			inspected, entry := inspectDesktopFile(candidate, "")
			if !entry.Valid || strings.TrimSpace(inspected.AccessToken) == "" {
				continue
			}
			if (realm == realmIntl || realm == realmCN) && entry.Realm != realm {
				continue
			}
			auth, err := ImportDesktopCredential(p.AccountsDir(), candidate.path, candidate.hint)
			if err != nil {
				continue
			}
			for _, provider := range p.allProviders() {
				provider.refreshSettings()
				provider.syncAccountFromDisk(auth.UID)
				provider.bestEffortInitialCheckin(r.Context(), auth.UID, taskTriggerAccountImport)
				provider.invalidateHealth()
			}
			imported = append(imported, p.accountViewsForUID(auth.UID)...)
		}
	}
	detected := make([]DesktopCredential, 0, len(candidates))
	for _, candidate := range candidates {
		_, entry := inspectDesktopFile(candidate, "")
		detected = append(detected, entry)
	}
	poolUIDs := make([]string, 0)
	for _, provider := range p.allProviders() {
		for _, account := range provider.Pool().Accounts() {
			poolUIDs = append(poolUIDs, account.UID())
		}
	}
	sort.Strings(poolUIDs)
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"imported":  imported,
		"detected":  detected,
		"accounts":  p.importAccountsViews(""),
		"pool_uids": poolUIDs,
		"atrest":    map[string]any{"available": false},
	})
}

func (p *Provider) accountViewsForUID(uid string) []AccountView {
	out := make([]AccountView, 0, 1)
	for _, provider := range p.allProviders() {
		if account := provider.Pool().Get(uid); account != nil {
			out = append(out, account.View())
		}
	}
	return out
}

func (p *Provider) handleAccountsLoginStart(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	realm := wbRequestRealm(r, payload, "")
	if realm == "" {
		realm = p.activeRealm()
	}
	provider := p.providerForRealm(realm)
	if provider == nil {
		writeWBAdminError(w, p.safeAdminRealmError(realm))
		return
	}
	platform := wbAdminText(payload["platform"])
	if platform == "" {
		platform = "CLI"
	}
	proxySlot := wbAdminText(payload["proxySlot"])
	if raw, present := payload["proxySlot"]; present && raw != nil {
		if _, ok := raw.(string); !ok {
			writeWBAdminError(w, wbBadRequest("proxySlot must be a string"))
			return
		}
	}
	started, err := provider.BeginOAuth(r.Context(), platform, proxySlot)
	if err != nil {
		if strings.Contains(err.Error(), "proxy slot") || strings.Contains(err.Error(), "proxySlot") {
			writeWBAdminError(w, wbBadRequest(err.Error()))
			return
		}
		writeWBAdminError(w, &wbAdminError{Status: http.StatusBadGateway, Type: "invalid_request_error", Message: "could not start login"})
		return
	}
	writeWBAdminJSON(w, http.StatusOK, started)
}

func (p *Provider) handleAccountsLoginPoll(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	realm := ""
	if r.Method == http.MethodPost {
		payload, failure := readWBAdminPayload(w, r)
		if failure != nil {
			writeWBAdminError(w, failure)
			return
		}
		state = wbAdminText(payload["state"])
		realm = wbAdminText(payload["realm"])
	} else {
		realm = r.URL.Query().Get("realm")
	}
	candidates := p.allProviders()
	if normalized, ok := normalizeRealm(realm); ok {
		if provider := p.providerForRealm(normalized); provider != nil {
			candidates = []*Provider{provider}
		}
	}
	if len(candidates) == 0 {
		writeWBAdminError(w, p.safeAdminRealmError(realm))
		return
	}
	var result OAuthPoll
	var selected *Provider
	for _, provider := range candidates {
		value, err := provider.PollOAuth(r.Context(), state)
		if err != nil {
			writeWBAdminError(w, &wbAdminError{Status: http.StatusBadGateway, Type: "invalid_request_error", Message: "login poll failed"})
			return
		}
		result = value
		selected = provider
		if value.Status != "unknown" {
			break
		}
	}
	if result.Status == "ok" && result.Account != nil {
		selected.syncAccountFromDisk(result.Account.UID)
		selected.refreshSettings()
		selected.bestEffortInitialCheckin(r.Context(), result.Account.UID, taskTriggerAccountAdd)
		selected.invalidateHealth()
	}
	writeWBAdminJSON(w, http.StatusOK, result)
}

func (p *Provider) handleAccountsLoginCancel(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	state := wbAdminText(payload["state"])
	realm := wbAdminText(payload["realm"])
	cancelled := false
	for _, provider := range p.allProviders() {
		if normalized, ok := normalizeRealm(realm); ok && provider.realm != normalized {
			continue
		}
		if provider.CancelOAuth(state) {
			cancelled = true
			break
		}
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{"cancelled": cancelled})
}

func (p *Provider) handleAccountsCheckin(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	provider := p.providerForRealm(realmCN)
	if provider == nil {
		writeWBAdminError(w, p.safeAdminRealmError(realmCN))
		return
	}
	provider.refreshSettings()
	before := p.accountViewsByUID()
	uid := wbAdminText(payload["uid"])
	results := make([]map[string]any, 0)
	targets := []*Account{}
	if uid != "" {
		if account := provider.Pool().Get(uid); account != nil && accountRealmOf(account) == realmCN {
			targets = []*Account{account}
		}
	} else {
		for _, account := range provider.Pool().TaskAccounts() {
			auth := taskAuthSnapshot(account)
			if auth.Realm != realmCN || taskIsAPIKey(auth) || taskEnterpriseID(auth) != "" {
				continue
			}
			targets = append(targets, account)
		}
	}
	for index, account := range targets {
		if index > 0 {
			provider.taskSleepFor(provider.taskGapDelay())
		}
		result, err := provider.runCheckin(r.Context(), account, taskTriggerManual)
		if err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		results = append(results, v1CheckinRow(result))
		provider.recordActivity(result.Activity)
	}
	provider.invalidateHealth()
	accounts := p.allAccountViews()
	// v1's check-in only persisted lastCheckin; it did not refresh the credit
	// snapshot or expose the newer check-in status in the account row. Keep the
	// pre-action snapshot so the response matches that contract.
	for index := range accounts {
		if previous, ok := before[accounts[index].UID]; ok {
			lastCheckin := accounts[index].LastCheckin
			accounts[index] = previous
			accounts[index].LastCheckin = lastCheckin
		}
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"results":  results,
		"accounts": accounts,
	})
}

// accountViewsByUID snapshots the whole pool before a panel action mutates it.
func (p *Provider) accountViewsByUID() map[string]AccountView {
	out := make(map[string]AccountView)
	for _, view := range p.allAccountViews() {
		out[view.UID] = view
	}
	return out
}

// v1CheckinRow projects the internal result onto v1's exact wire shape. v1
// returned uid/nickname plus ok/state/code/msg/data on success and
// ok/state/code/msg/error on failure; realm, skipped, busy, activity, checkin
// and credits were never part of the check-in response.
func v1CheckinRow(result CheckinResult) map[string]any {
	row := map[string]any{
		"uid":      result.UID,
		"nickname": result.Nickname,
		"ok":       result.OK,
	}
	if result.State != "" {
		row["state"] = result.State
	}
	if result.Code != nil {
		row["code"] = result.Code
	}
	if result.Msg != "" {
		row["msg"] = result.Msg
	}
	if result.Error != "" {
		row["error"] = result.Error
	}
	if result.OK {
		row["data"] = result.Data
	}
	return row
}

func (p *Provider) handleAccountsDailyChat(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	provider := p.providerForRealm(realmIntl)
	if provider == nil {
		writeWBAdminError(w, p.safeAdminRealmError(realmIntl))
		return
	}
	provider.refreshSettings()
	targets, failure := provider.dailyTargets(payload)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	results := make([]DailyResult, 0)
	for index, account := range targets {
		if index > 0 {
			provider.taskSleepFor(provider.taskGapDelay())
		}
		result, err := provider.runDailyActivity(r.Context(), account, taskTriggerManual)
		if err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		results = append(results, result)
	}
	provider.invalidateHealth()
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"results":  results,
		"accounts": p.allAccountViews(),
	})
}

// runDailyActivity is v1's daily_chat flow: desktop identity leg first, then
// the web conversation leg when the setting is enabled and the desktop leg
// actually succeeded. A desktop account-level restriction skips the web leg.
// Both activity events are recorded here (Task 8 de-duplicates per day).
func (p *Provider) runDailyActivity(ctx context.Context, account *Account, trigger string) (DailyResult, error) {
	auth := taskAuthSnapshot(account)
	previousLastDailyChat := auth.LastDailyChat
	result, err := p.runDailyChat(ctx, account, trigger)
	if err != nil {
		p.recordDailyActivity(result.Activity)
		return result, err
	}
	if result.Restricted || !result.OK || result.Skipped || !p.dailyChatWebEnabled() {
		p.recordDailyActivity(result.Activity)
		return result, nil
	}
	webRunner := p.runDailyChatWeb
	if p.dailyWebRun != nil {
		webRunner = p.dailyWebRun
	}
	web, webErr := webRunner(ctx, account, trigger)
	result.Web = &DailyWebResult{
		OK: web.OK, Skipped: web.Skipped, Busy: web.Busy, Conversation: web.Conversation,
		Status: web.Status, Model: web.Model, Events: web.Events, Chunks: web.Chunks,
		ElapsedMS: web.ElapsedMS, Attempts: web.Attempts, ModelsTried: web.ModelsTried,
		Phase: web.Phase, BudgetExhausted: web.BudgetExhausted, Retryable: web.Retryable,
		Msg: web.Msg, Error: web.Error,
	}
	now := p.taskNowTime()
	if web.Busy {
		// Another task currently owns the account. Propagate Busy rather than
		// converting it to a web failure or rolling back the desktop stamp.
		result.Busy = true
		result.OK = false
		result.Error = ""
		result.Msg = firstNonEmpty(web.Msg, taskBusyMessage)
		return result, nil
	}
	if webErr != nil || !web.OK {
		message := firstNonEmpty(web.Error, "未知错误")
		if webErr != nil {
			message = safeJobError(webErr)
		}
		result.OK = false
		result.Error = "网页通道失败：" + message
		result.Msg = "每日活跃对话未完成（" + result.Error + "）"
		// The desktop leg stamped LastDailyChat before the web result existed.
		// A failed web leg must remain retryable, so put the previous stamp back
		// and mark the daily activity outcome as failed.
		p.restoreLastDailyChat(account, previousLastDailyChat)
		_ = p.persistDailyEventMarker(account, now, false)
		result.Activity = p.newActivity(account, taskDailyChatTask, trigger, false, result.Msg)
		p.recordDailyActivity(result.Activity)
		return result, nil
	}
	message := result.Msg
	if !web.Skipped {
		message = fmt.Sprintf("每日活跃对话成功完成（网页通道 %s：%d 段输出，%d ms）",
			firstNonEmpty(web.Status, "completed"), web.Chunks, web.ElapsedMS)
	}
	result.Msg = message
	result.Activity = p.newActivity(account, taskDailyChatTask, trigger, true, message)
	_ = p.persistDailyEventMarker(account, now, true)
	p.recordDailyActivity(result.Activity)
	return result, nil
}

func (p *Provider) restoreLastDailyChat(account *Account, previous string) {
	if p == nil || account == nil {
		return
	}
	uid := account.UID()
	if uid == "" || p.Pool() == nil {
		account.mu.Lock()
		account.Auth.LastDailyChat = previous
		account.mu.Unlock()
		return
	}
	_ = p.Pool().mutateAccount(uid, func(account *Account) {
		account.Auth.LastDailyChat = previous
	})
}

func (p *Provider) dailyTargets(payload map[string]any) ([]*Account, *wbAdminError) {
	uid := wbAdminText(payload["uid"])
	if uid != "" {
		account := p.Pool().Get(uid)
		if account == nil || accountRealmOf(account) != realmIntl {
			return []*Account{}, nil
		}
		return []*Account{account}, nil
	}
	accounts := p.Pool().TaskAccounts()
	wanted := map[string]bool{}
	restrict := false
	if raw, present := payload["uids"]; present {
		values, ok := wbAdminStringList(raw)
		if !ok {
			return nil, wbBadRequest("uids must be a list")
		}
		restrict = true
		for _, item := range values {
			wanted[item] = true
		}
	}
	out := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		if accountRealmOf(account) != realmIntl {
			continue
		}
		if restrict && !wanted[account.UID()] {
			continue
		}
		out = append(out, account)
	}
	return out, nil
}

func (p *Provider) handleAccountsDailyChatWeb(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	provider := p.providerForRealm(realmIntl)
	if provider == nil {
		writeWBAdminError(w, p.safeAdminRealmError(realmIntl))
		return
	}
	provider.refreshSettings()
	targets, failure := provider.dailyTargets(payload)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	results := make([]DailyResult, 0)
	for index, account := range targets {
		if index > 0 {
			provider.taskSleepFor(provider.taskGapDelay())
		}
		result, err := provider.runDailyChatWebManual(r.Context(), account, taskTriggerManual)
		if err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		results = append(results, result)
		provider.recordDailyActivity(result.Activity)
	}
	provider.invalidateHealth()
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"results":  results,
		"accounts": provider.Pool().List(),
	})
}

func (p *Provider) handleTasks(w http.ResponseWriter, r *http.Request) {
	provider := p.providerForRealm(realmCN)
	if provider == nil {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"tasks": []GrowthTask{}, "summary": GrowthSummary{}, "accounts": []any{},
			"msg": "未找到参与任务的国内版账号",
		})
		return
	}
	provider.refreshSettings()
	taskAccounts := provider.Pool().TaskAccounts()
	targets := make([]*Account, 0, len(taskAccounts))
	for _, account := range taskAccounts {
		if accountRealmOf(account) == realmCN {
			targets = append(targets, account)
		}
	}
	if len(targets) == 0 {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"tasks": []GrowthTask{}, "summary": GrowthSummary{}, "accounts": []any{},
			"msg": "未找到参与任务的国内版账号",
		})
		return
	}
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	var selected *Account
	if uid != "" && uid != "all" {
		if account := provider.Pool().Get(uid); account != nil && accountRealmOf(account) == realmCN {
			selected = account
		}
	}
	if selected == nil {
		selected = targets[0]
	}
	snapshot, err := provider.taskSnapshotForPanel(r.Context(), selected)
	if err != nil {
		writeWBAdminError(w, wbAdminErrorFrom(err))
		return
	}
	accounts := make([]map[string]any, 0, len(targets))
	for _, account := range targets {
		accounts = append(accounts, map[string]any{
			"uid":      account.UID(),
			"nickname": firstNonEmpty(accountNicknameOf(account), shortUID(account.UID())),
		})
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"tasks":    snapshot.Tasks,
		"summary":  snapshot.Summary,
		"account":  selected.View(),
		"accounts": accounts,
	})
}

// taskSnapshotForPanel returns the cached growth summary, or fetches a fresh
// one when an action invalidated the snapshot. v1's invalidate_tasks_cache did
// the same: the next panel read goes upstream instead of serving stale state.
func (p *Provider) taskSnapshotForPanel(ctx context.Context, account *Account) (GrowthResult, error) {
	result, err := p.GrowthSnapshot(ctx, account.UID())
	if err != nil {
		return result, err
	}
	if result.Msg == "cached" {
		return result, nil
	}
	auth := taskAuthSnapshot(account)
	if !growthTaskEligible(auth) || !account.taskMu.TryLock() {
		return result, nil
	}
	defer account.taskMu.Unlock()
	tasks, fetchErr := p.fetchGrowthTasks(ctx, account, false)
	if fetchErr != nil {
		return result, nil
	}
	summary, summaryErr := p.fetchGrowthSummary(ctx, account, cachedGrowthSummary(auth))
	if summaryErr != nil {
		summary = cachedGrowthSummary(auth)
	}
	run := &growthRun{
		provider: p, account: account, auth: auth, now: p.taskNowTime(),
		autoClaim: true, tasks: tasks, summary: summary,
		claims: loadGrowthClaims(auth),
	}
	if persistErr := p.persistGrowthState(account, run); persistErr != nil {
		return result, nil
	}
	refreshed, refreshErr := p.GrowthSnapshot(ctx, account.UID())
	if refreshErr != nil {
		return result, nil
	}
	return refreshed, nil
}

func (p *Provider) handleTasksRun(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	provider := p.providerForRealm(realmCN)
	if provider == nil {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "账号池不可用"})
		return
	}
	provider.refreshSettings()
	uid := wbAdminText(payload["uid"])
	taskAccounts := provider.Pool().TaskAccounts()
	targets := make([]*Account, 0, len(taskAccounts))
	for _, account := range taskAccounts {
		if accountRealmOf(account) == realmCN {
			targets = append(targets, account)
		}
	}
	if uid != "" && uid != "all" {
		account := provider.Pool().Get(uid)
		if account == nil || accountRealmOf(account) != realmCN {
			writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "未找到指定的国内版账号"})
			return
		}
		targets = []*Account{account}
	}
	if len(targets) == 0 {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "未找到参与任务的国内版账号"})
		return
	}
	autoClaim := true
	if raw, present := payload["claim"]; present {
		if value, ok := wbAdminBool(raw); ok {
			autoClaim = value
		}
	}
	results := make([]GrowthResult, 0, len(targets))
	for index, account := range targets {
		if index > 0 {
			provider.taskSleepFor(provider.taskGapDelay())
		}
		result, err := provider.runGrowth(r.Context(), account, autoClaim, taskTriggerManual)
		if err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		results = append(results, result)
	}
	logs := make([]string, 0)
	total := 0.0
	for index, result := range results {
		nick := firstNonEmpty(result.Nickname, shortUID(result.UID))
		logs = append(logs, fmt.Sprintf("====== 正在为账号 [%s (%s)] 执行全自动成长任务 (%d/%d) ======", nick, result.UID, index+1, len(results)))
		for _, line := range result.Logs {
			logs = append(logs, "  "+line)
		}
		total += result.EarnedCredit
	}
	logs = append(logs, fmt.Sprintf("====== 全部 %d 个账号任务执行完毕，累计新增积分: +%g ======", len(results), total))
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"credit_added":   total,
		"logs":           logs,
		"accounts_count": len(results),
	})
}

func (p *Provider) handleTasksClaim(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	provider := p.providerForRealm(realmCN)
	if provider == nil {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "账号池不可用"})
		return
	}
	uid := wbAdminText(payload["uid"])
	code := wbAdminText(payload["code"])
	if uid == "" || uid == "all" {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "请先选择一个国内版账号"})
		return
	}
	if code == "" {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "缺少任务代码"})
		return
	}
	account := provider.Pool().Get(uid)
	if account == nil || accountRealmOf(account) != realmCN {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "未找到指定的国内版账号"})
		return
	}
	if !account.taskMu.TryLock() {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": taskBusyMessage})
		return
	}
	defer account.taskMu.Unlock()
	auth := taskAuthSnapshot(account)
	credit, energy, ok, message, err := provider.claimGrowthTaskUpstream(r.Context(), account, code, false)
	msg := message
	if err != nil {
		msg = provider.taskRedactError(account, err)
		ok = false
	}
	if ok {
		claims := loadGrowthClaims(auth)
		claims[code] = growthClaimEntry{
			Date:   provider.taskNowTime().Format(tasksDayLayout),
			TS:     provider.taskNowTime().Format(tasksTimeLayout),
			Credit: credit, Energy: energy,
		}
		if raw, marshalErr := json.Marshal(claims); marshalErr == nil {
			_ = provider.Pool().mutateAccount(uid, func(account *Account) {
				if account.Auth.Extra == nil {
					account.Auth.Extra = map[string]json.RawMessage{}
				}
				account.Auth.Extra["growthClaims"] = raw
			})
		}
		provider.invalidateGrowthSnapshot(uid)
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok":     ok,
		"msg":    firstNonEmpty(msg, map[bool]string{true: "领取成功", false: "领取失败"}[ok]),
		"credit": credit,
	})
}

func (p *Provider) taskTargets(uid string) (*Provider, []*Account, string) {
	provider := p.providerForRealm(realmCN)
	if provider == nil {
		return nil, nil, "账号池不可用"
	}
	provider.refreshSettings()
	if uid != "" && uid != "all" {
		account := provider.Pool().Get(uid)
		if account == nil || accountRealmOf(account) != realmCN {
			return provider, nil, "未找到指定的国内版账号"
		}
		return provider, []*Account{account}, ""
	}
	accounts := provider.Pool().TaskAccounts()
	targets := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		if accountRealmOf(account) == realmCN {
			targets = append(targets, account)
		}
	}
	if len(targets) == 0 {
		return provider, nil, "未找到参与任务的国内版账号"
	}
	return provider, targets, ""
}

func (p *Provider) handleTasksTravel(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	provider, targets, message := p.taskTargets(wbAdminText(payload["uid"]))
	if message != "" {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": message})
		return
	}
	results := make([]map[string]any, 0, len(targets))
	lines := make([]string, 0, len(targets))
	for index, account := range targets {
		if index > 0 {
			provider.taskSleepFor(provider.taskGapDelay())
		}
		result, err := provider.runCatTravel(r.Context(), account)
		if err != nil {
			result = CatResult{UID: account.UID(), Error: provider.taskRedactError(account, err)}
		}
		nick := firstNonEmpty(result.Nickname, shortUID(result.UID))
		results = append(results, map[string]any{
			"uid": result.UID, "nickname": nick, "action": result.Action,
			"msg": result.Msg, "reward_credit": result.Credit,
		})
		lines = append(lines, fmt.Sprintf("%s: %s", nick, firstNonEmpty(result.Msg, result.Error)))
		provider.invalidateGrowthSnapshot(account.UID())
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok": true, "results": results, "msg": strings.Join(lines, "\n"),
		"accounts_count": len(results),
	})
}

func (p *Provider) handleTasksStreakBonus(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	provider, targets, message := p.taskTargets(wbAdminText(payload["uid"]))
	if message != "" {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": message})
		return
	}
	lines := make([]string, 0, len(targets))
	for index, account := range targets {
		if index > 0 {
			provider.taskSleepFor(provider.taskGapDelay())
		}
		result, err := provider.runStreakBonus(r.Context(), account)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s: %s", firstNonEmpty(accountNicknameOf(account), shortUID(account.UID())), provider.taskRedactError(account, err)))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", firstNonEmpty(result.Nickname, shortUID(result.UID)), firstNonEmpty(result.Msg, result.Error)))
		provider.invalidateGrowthSnapshot(account.UID())
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": strings.Join(lines, "\n")})
}

func (p *Provider) handleTasksBuddy(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	provider, targets, message := p.taskTargets(wbAdminText(payload["uid"]))
	if message != "" {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": message})
		return
	}
	results := make([]BuddyResult, 0, len(targets))
	for index, account := range targets {
		if index > 0 {
			provider.taskSleepFor(provider.taskGapDelay())
		}
		result, err := provider.runBuddyCheckin(r.Context(), account)
		if err != nil {
			result = BuddyResult{UID: account.UID(), Error: provider.taskRedactError(account, err)}
		}
		results = append(results, result)
		provider.invalidateGrowthSnapshot(account.UID())
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

func (p *Provider) handleCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	realm := wbRequestRealm(r, payload, p.realm)
	if realm == "all" {
		results := make([]CatalogResult, 0, 2)
		for _, provider := range p.allProviders() {
			provider.refreshSettings()
			result, err := provider.RefreshCatalog(r.Context())
			if err != nil {
				writeWBAdminError(w, wbAdminErrorFrom(err))
				return
			}
			results = append(results, result)
			provider.invalidateHealth()
		}
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "realm": "all", "results": results})
		return
	}
	provider := p.providerForRealm(realm)
	if provider == nil {
		writeWBAdminError(w, p.safeAdminRealmError(realm))
		return
	}
	provider.refreshSettings()
	result, err := provider.RefreshCatalog(r.Context())
	if err != nil {
		writeWBAdminError(w, wbAdminErrorFrom(err))
		return
	}
	provider.invalidateHealth()
	writeWBAdminJSON(w, http.StatusOK, result)
}

func (p *Provider) handleScheduler(w http.ResponseWriter, r *http.Request) {
	writeWBAdminJSON(w, http.StatusOK, p.SchedulerStatus())
}

// handleSchedulerTrigger runs the panel's manual sweep synchronously so the
// caller gets deterministic per-account results. Unlike v1's fire-and-forget
// thread this can hold the request for the duration of the tasks, but it keeps
// failures visible and avoids a second overlapping cycle.
func (p *Provider) handleSchedulerTrigger(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	name := wbAdminText(payload["job"])
	force := true
	if raw, present := payload["force"]; present {
		if value, ok := wbAdminBool(raw); ok {
			force = value
		}
	}
	results, err := p.RunJobs(r.Context(), name, force)
	if errors.Is(err, errSchedulerBusy) {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"ok": false, "msg": "已有巡检正在执行，请稍候再试", "synchronous": true,
		})
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"ok": false, "results": results, "msg": "部分账号任务失败", "synchronous": true,
		})
		return
	}
	if err != nil {
		writeWBAdminError(w, wbAdminErrorFrom(err))
		return
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok": true, "msg": "已触发后台调度执行", "results": results, "synchronous": true,
	})
}

func (p *Provider) handleSchedulerToggle(w http.ResponseWriter, r *http.Request) {
	p.ToggleScheduler()
	writeWBAdminJSON(w, http.StatusOK, p.SchedulerStatus())
}

func (p *Provider) handleAccountsSet(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	p.handleAccountPatch(w, r, payload, "")
}

func (p *Provider) handleAccountPatch(w http.ResponseWriter, r *http.Request, payload map[string]any, forcedAction string) {
	uid := wbAdminText(payload["uid"])
	if uid == "" {
		writeWBAdminError(w, wbBadRequest("uid required"))
		return
	}
	realm := wbRequestRealm(r, payload, "")
	provider, account := p.providerForUID(uid, realm)
	if account == nil {
		writeWBAdminError(w, &wbAdminError{Status: http.StatusNotFound, Type: "invalid_request_error", Message: "no such account"})
		return
	}
	recognized := false
	if forcedAction != "" {
		payload = map[string]any{forcedAction: payload[forcedAction]}
		if forcedAction == "enabled" {
			payload = map[string]any{"enabled": payload["enabled"]}
		}
		recognized = true
	}
	if _, present := payload["enabled"]; present || forcedAction == "enabled" {
		enabled, ok := wbAdminBool(payload["enabled"])
		if !ok {
			writeWBAdminError(w, wbBadRequest("enabled must be true or false"))
			return
		}
		var err error
		if enabled {
			err = provider.Pool().Enable(uid)
		} else {
			reason := firstNonEmpty(wbAdminText(payload["reason"]), "manual disable")
			err = provider.Pool().Disable(uid, reason)
		}
		if err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		recognized = true
	}
	if raw, present := payload["tasksEnabled"]; present {
		enabled, ok := wbAdminBool(raw)
		if !ok {
			writeWBAdminError(w, wbBadRequest("tasksEnabled must be true or false"))
			return
		}
		if err := provider.Pool().SetTasksEnabled(uid, enabled); err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		recognized = true
	}
	if raw, present := payload["note"]; present {
		if err := provider.Pool().SetNote(uid, wbAdminText(raw)); err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		recognized = true
	}
	if raw, present := payload["weight"]; present {
		weight, ok := wbAdminInt(raw)
		if !ok || weight < minWeight || weight > maxWeight {
			writeWBAdminError(w, wbBadRequest("weight must be an integer between 1 and 100"))
			return
		}
		if err := provider.Pool().SetWeight(uid, float64(weight)); err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		recognized = true
	}
	if raw, present := payload["group"]; present {
		if err := provider.Pool().SetGroup(uid, wbAdminText(raw)); err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		recognized = true
	}
	if raw, present := payload["disabledModels"]; present {
		models, ok := wbAdminStringList(raw)
		if !ok {
			writeWBAdminError(w, wbBadRequest("disabledModels must be a list of model ids"))
			return
		}
		if err := provider.Pool().SetDisabledModels(uid, models); err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		recognized = true
	}
	if raw, present := payload["proxySlot"]; present {
		if err := provider.Pool().SetProxySlot(uid, wbAdminText(raw)); err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		recognized = true
	}
	if raw, present := payload["proxy"]; present {
		proxyURL := wbAdminText(raw)
		err := provider.Pool().mutateAccount(uid, func(account *Account) {
			if account.Auth.Extra == nil {
				account.Auth.Extra = map[string]json.RawMessage{}
			}
			if proxyURL == "" {
				delete(account.Auth.Extra, "proxy")
				account.Auth.ProxyURL = ""
			} else {
				encoded, _ := json.Marshal(proxyURL)
				account.Auth.Extra["proxy"] = encoded
				account.Auth.ProxyURL = proxyURL
			}
		})
		if err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		recognized = true
	}
	if !recognized {
		writeWBAdminError(w, wbBadRequest("nothing to update: pass 'enabled', 'tasksEnabled', 'weight', 'note', 'group', 'disabledModels', 'proxySlot' or 'proxy'"))
		return
	}
	provider.invalidateHealth()
	writeWBAdminJSON(w, http.StatusOK, map[string]any{"account": account.View()})
}

func (p *Provider) handleAccountsSetAll(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	enabled, ok := payload["enabled"].(bool)
	if !ok {
		writeWBAdminError(w, wbBadRequest("enabled must be true or false"))
		return
	}
	realm := wbRequestRealm(r, payload, "")
	providersList, failure := p.routeProviders(realm, "")
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	group := wbAdminText(payload["group"])
	for _, provider := range providersList {
		provider.refreshSettings()
		if group != "" {
			if _, err := provider.Pool().SetGroupEnabled(group, enabled); err != nil {
				writeWBAdminError(w, wbAdminErrorFrom(err))
				return
			}
		} else {
			for _, account := range provider.Pool().Accounts() {
				uid := account.UID()
				var err error
				if enabled {
					err = provider.Pool().Enable(uid)
				} else {
					err = provider.Pool().Disable(uid, "manual disable")
				}
				if err != nil {
					writeWBAdminError(w, wbAdminErrorFrom(err))
					return
				}
			}
		}
		provider.invalidateHealth()
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{"accounts": p.importAccountsViews(realm)})
}

func (p *Provider) handleAccountsProduct(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	target := strings.ToLower(strings.TrimSpace(wbAdminText(payload["product"])))
	if target != productDesktop && target != productVSCode && target != productCLI {
		writeWBAdminError(w, wbBadRequest("product must be 'workbuddy', 'vscode', or 'cli'"))
		return
	}
	uid := wbAdminText(payload["uid"])
	realmSelector := wbRequestRealm(r, payload, "")
	if uid == "" && realmSelector == "" {
		realmSelector = "all"
	}
	providersList, failure := p.routeProviders(realmSelector, "all")
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	if uid != "" {
		provider, account := p.providerForUID(uid, realmSelector)
		if account == nil {
			writeWBAdminJSON(w, http.StatusOK, map[string]any{
				"ok": true, "product": target, "changed": []string{},
				"accounts": accountViewsForProviders(p.allProviders()),
			})
			return
		}
		providersList = []*Provider{provider}
	}
	changed := make([]string, 0)
	for _, provider := range providersList {
		for _, account := range provider.Pool().Accounts() {
			accountID := account.UID()
			switched, err := provider.Pool().SetProduct(accountID, target)
			if err != nil || !switched {
				continue
			}
			// v1 clear_error(keep_caps=True): clear runtime limit/error state,
			// leave persisted modelCaps (quota windows) untouched.
			_ = provider.Pool().ClearCooldown(accountID)
			changed = append(changed, accountID[:wbMinInt(8, len(accountID))])
		}
		provider.invalidateHealth()
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok": true, "product": target, "changed": changed,
		"accounts": accountViewsForProviders(providersList),
	})
}

func accountViewsForProviders(providersList []*Provider) []AccountView {
	out := make([]AccountView, 0)
	for _, provider := range providersList {
		if pool := provider.Pool(); pool != nil {
			out = append(out, pool.List()...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

func (p *Provider) handleAccountsDelete(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	uid := wbAdminText(payload["uid"])
	if uid == "" {
		writeWBAdminError(w, wbBadRequest("uid required"))
		return
	}
	provider, account := p.providerForUID(uid, wbRequestRealm(r, payload, ""))
	if account == nil {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"deleted": false, "accounts": p.importAccountsViews("")})
		return
	}
	if err := provider.Pool().Remove(uid); err != nil {
		writeWBAdminError(w, wbAdminErrorFrom(err))
		return
	}
	provider.invalidateHealth()
	writeWBAdminJSON(w, http.StatusOK, map[string]any{"deleted": true, "accounts": p.importAccountsViews("")})
}

func (p *Provider) handleAccountsRefresh(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	uid := wbAdminText(payload["uid"])
	realmSelector := wbRequestRealm(r, payload, "")
	providersList, failure := p.routeProviders(realmSelector, "")
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	if uid != "" {
		provider, account := p.resolveAccountUID(uid, realmSelector)
		if account == nil {
			writeWBAdminJSON(w, http.StatusOK, map[string]any{"results": []map[string]any{}})
			return
		}
		providersList = []*Provider{provider}
	}
	group := wbAdminText(payload["group"])
	results := make([]map[string]any, 0)
	for _, provider := range providersList {
		provider.refreshSettings()
		targets := provider.Pool().Accounts()
		if uid != "" {
			if account := provider.Pool().Get(uid); account != nil {
				targets = []*Account{account}
			} else {
				continue
			}
		}
		for _, account := range targets {
			if group != "" && accountGroupOf(account) != group {
				continue
			}
			auth := taskAuthSnapshot(account)
			row := map[string]any{"uid": auth.UID, "ok": false, "error": ""}
			if taskIsAPIKey(auth) || strings.TrimSpace(auth.RefreshToken) == "" {
				row["error"] = "account has no refresh token"
				results = append(results, row)
				continue
			}
			if err := provider.ForceRefresh(r.Context(), account); err != nil {
				row["error"] = provider.redactTokenErrorText(account, err.Error())
			} else {
				row["ok"] = true
				row["error"] = ""
			}
			results = append(results, row)
		}
		provider.invalidateHealth()
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (p *Provider) handleAccountsSyncProfile(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	uid := wbAdminText(payload["uid"])
	realmSelector := wbRequestRealm(r, payload, "")
	providersList, failure := p.routeProviders(realmSelector, "")
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	if uid != "" {
		provider, account := p.resolveAccountUID(uid, realmSelector)
		if account == nil {
			writeWBAdminJSON(w, http.StatusOK, map[string]any{
				"updated": []map[string]any{}, "failed": []map[string]any{},
				"accounts": p.importAccountsViews(realmSelector),
			})
			return
		}
		providersList = []*Provider{provider}
	}
	updated := make([]map[string]any, 0)
	failed := make([]map[string]any, 0)
	for _, provider := range providersList {
		provider.refreshSettings()
		targets := provider.Pool().Accounts()
		if uid != "" {
			if account := provider.Pool().Get(uid); account != nil {
				targets = []*Account{account}
			} else {
				continue
			}
		}
		for _, account := range targets {
			auth := taskAuthSnapshot(account)
			if taskIsAPIKey(auth) {
				failed = append(failed, map[string]any{"uid": auth.UID, "error": "API key accounts have no web profile"})
				continue
			}
			if err := FetchProfile(r.Context(), &auth); err != nil {
				failed = append(failed, map[string]any{"uid": auth.UID, "error": "profile request failed"})
				continue
			}
			if err := provider.Pool().mutateAccount(auth.UID, func(account *Account) {
				account.Auth.Nickname = auth.Nickname
			}); err != nil {
				failed = append(failed, map[string]any{"uid": auth.UID, "error": "profile save failed"})
				continue
			}
			updated = append(updated, map[string]any{"uid": auth.UID, "nickname": auth.Nickname})
		}
		provider.invalidateHealth()
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"updated": updated, "failed": failed, "accounts": p.importAccountsViews(realmSelector),
	})
}

func (p *Provider) handleAccountsTest(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	uid := wbAdminText(payload["uid"])
	model := firstNonEmpty(wbAdminText(payload["model"]), taskDailyDefaultModel)
	if uid != "" {
		provider, account := p.providerForUID(uid, wbRequestRealm(r, payload, ""))
		if account == nil {
			writeWBAdminError(w, &wbAdminError{Status: http.StatusNotFound, Type: "invalid_request_error", Message: "no such account"})
			return
		}
		wire, err := provider.probeAccount(r.Context(), account, model)
		if err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		writeWBAdminJSON(w, http.StatusOK, wire)
		return
	}
	realm := wbRequestRealm(r, payload, "")
	if realm == "" {
		realm = p.activeRealm()
	}
	provider := p.providerForRealm(realm)
	if provider == nil {
		writeWBAdminError(w, p.safeAdminRealmError(realm))
		return
	}
	provider.refreshSettings()
	started := time.Now()
	rows := make([]map[string]any, 0)
	group := wbAdminText(payload["group"])
	targets := provider.Pool().Accounts()
	for index, account := range targets {
		if !accountEnabledForTest(account) {
			continue
		}
		if group != "" && accountGroupOf(account) != group {
			continue
		}
		if index > 0 {
			provider.taskSleepFor(provider.taskGapDelay())
		}
		wire, err := provider.probeAccount(r.Context(), account, model)
		if err != nil {
			wire = map[string]any{
				"ok": false, "uid": account.UID(), "model": model,
				"status": http.StatusInternalServerError, "error": wbSafeErrorText(err),
				"network": false, "realm": accountRealmOf(account),
			}
		}
		rows = append(rows, wire)
	}
	passed, failedCount := 0, 0
	for _, row := range rows {
		if row["ok"] == true {
			passed++
		} else {
			failedCount++
		}
	}
	network := provider.probeNetwork(r.Context(), realm)
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"summary": map[string]any{
			"realm": realm, "model": model, "tested": len(rows),
			"passed": passed, "failed": failedCount, "networkDown": network["ok"] == false,
			"elapsed_ms": time.Since(started).Milliseconds(),
		},
		"network":  network,
		"results":  rows,
		"accounts": provider.Pool().List(),
	})
}

func (p *Provider) probeNetwork(ctx context.Context, realm string) map[string]any {
	sourceURL := strings.TrimSpace(p.networkURL)
	if sourceURL == "" {
		sourceURL = p.chatURLFor(productDesktop)
	}
	info := map[string]any{
		"realm": realm, "url": sourceURL, "host": "", "ip": "", "addresses": []string{},
		"ok": false, "dns_ms": nil, "tcp_ms": nil, "error": "",
	}
	parsed, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil || parsed.Hostname() == "" {
		info["error"] = "upstream URL has no host"
		return info
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		info["error"] = "上游地址必须是 https + 主机名：" + sourceURL
		return info
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	info["host"] = host
	probeCtx, cancel := context.WithTimeout(nonNilContext(ctx), 5*time.Second)
	defer cancel()
	started := time.Now()
	lookup := p.networkLookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	addresses, err := lookup(probeCtx, host)
	info["dns_ms"] = time.Since(started).Milliseconds()
	if err != nil || len(addresses) == 0 {
		if err == nil {
			err = errors.New("no DNS addresses")
		}
		info["error"] = "DNS 解析失败：" + safeJobError(err)
		return info
	}
	info["addresses"] = append([]string(nil), addresses...)
	public := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if !isPublicAddress(address) {
			info["error"] = "解析到非公网地址，已拒绝连接：" + address
			return info
		}
		public = append(public, address)
	}
	dial := p.networkDial
	if dial == nil {
		dialer := &net.Dialer{Timeout: 3 * time.Second}
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		}
	}
	var lastErr error
	for _, address := range public {
		started = time.Now()
		conn, dialErr := dial(probeCtx, "tcp", net.JoinHostPort(address, port))
		info["tcp_ms"] = time.Since(started).Milliseconds()
		if dialErr != nil {
			lastErr = dialErr
			continue
		}
		_ = conn.Close()
		info["ip"] = address
		info["ok"] = true
		return info
	}
	if lastErr == nil {
		lastErr = errors.New("no dialable address")
	}
	info["ip"] = public[0]
	info["error"] = "TCP 连接失败：" + safeJobError(lastErr)
	return info
}

// isPublicAddress mirrors v1's probe guard: loopback, private, link-local,
// multicast, unspecified, and otherwise non-global addresses are never dialed.
func isPublicAddress(address string) bool {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil {
		return false
	}
	return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified()
}

func (p *Provider) probeAccount(ctx context.Context, account *Account, model string) (map[string]any, error) {
	started := time.Now()
	auth := taskAuthSnapshot(account)
	row := map[string]any{
		"uid": auth.UID, "nickname": auth.Nickname, "realm": auth.Realm,
		"model": model, "ok": false, "elapsed_ms": int64(0),
		"status": any(nil), "reply": "", "error": "", "network": false,
	}
	body := map[string]any{
		"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream": true, "max_tokens": 8,
	}
	raw := map[string]json.RawMessage{}
	for key, value := range body {
		encoded, err := json.Marshal(value)
		if err != nil {
			return row, err
		}
		raw[key] = encoded
	}
	req := providers.ChatRequest{
		Raw: raw, Headers: http.Header{}, Model: model,
		Messages: []json.RawMessage{raw["messages"]},
	}
	probeCtx, cancel := context.WithTimeout(nonNilContext(ctx), 20*time.Second)
	defer cancel()
	if err := p.EnsureToken(probeCtx, account); err != nil {
		row["elapsed_ms"] = time.Since(started).Milliseconds()
		row["status"] = http.StatusBadGateway
		row["error"] = p.redactTokenErrorText(account, err.Error())
		account.mu.RLock()
		row["fails"] = account.State.ErrCount
		account.mu.RUnlock()
		return row, nil
	}
	cfg := p.refreshChatConfig()
	baseBody, resolvedModel, err := p.buildChatBody(req)
	if err != nil {
		row["elapsed_ms"] = time.Since(started).Milliseconds()
		row["status"] = http.StatusBadRequest
		row["error"] = wbSafeErrorText(err)
		return row, nil
	}
	if !p.Pool().Acquire(account) {
		row["elapsed_ms"] = time.Since(started).Milliseconds()
		row["status"] = http.StatusTooManyRequests
		row["error"] = "account concurrency is full"
		return row, nil
	}
	send := func() (*chatStream, error) {
		return p.sendChatOnce(probeCtx, account, baseBody, resolvedModel, req, cfg,
			newTurnRequestID(), newMessageID())
	}
	stream, err := send()
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr != nil && apiErr.Kind == KindAuth {
			// A probe is explicitly allowed one credential refresh, exactly like
			// the chat path's second 401 attempt. API keys and accounts without a
			// refresh token are not refreshable; a no-op refresh that leaves the
			// access token unchanged must not produce a second identical 401.
			auth := taskAuthSnapshot(account)
			if !taskIsAPIKey(auth) && strings.TrimSpace(auth.RefreshToken) != "" {
				before := auth.AccessToken
				if refreshErr := p.ForceRefresh(probeCtx, account); refreshErr == nil {
					if after := taskAuthSnapshot(account).AccessToken; after != "" && after != before {
						stream, err = send()
					}
				} else {
					err = refreshErr
				}
			}
		}
	}
	if err != nil {
		p.Pool().Release(account)
		row["elapsed_ms"] = time.Since(started).Milliseconds()
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr != nil {
			row["status"] = apiErr.Status
			row["error"] = firstNonEmpty(apiErr.SafeMessage(), "request failed")
			if apiErr.Kind == KindNetwork {
				row["network"] = true
				row["status"] = http.StatusServiceUnavailable
			} else if apiErr.Kind == KindRestricted {
				_ = p.Pool().NoteRestricted(account, apiErr.SafeMessage())
			} else if apiErr.Kind != KindInvalid && apiErr.Kind != KindContentRejected {
				_ = p.Pool().NoteError(account, apiErr.SafeMessage(), false, false)
			}
		} else {
			row["status"] = http.StatusBadGateway
			row["error"] = wbSafeErrorText(err)
		}
		account.mu.RLock()
		row["fails"] = account.State.ErrCount
		account.mu.RUnlock()
		return row, nil
	}
	defer stream.Close()
	reply, readErr := aggregateProbeReply(stream)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		row["elapsed_ms"] = time.Since(started).Milliseconds()
		var apiErr *APIError
		if errors.As(readErr, &apiErr) && apiErr != nil {
			row["status"] = apiErr.Status
			row["error"] = firstNonEmpty(apiErr.SafeMessage(), "upstream chat stream aborted")
			if apiErr.Kind == KindNetwork {
				row["network"] = true
			}
		} else {
			row["status"] = http.StatusBadGateway
			row["error"] = wbSafeErrorText(readErr)
		}
		account.mu.RLock()
		row["fails"] = account.State.ErrCount
		account.mu.RUnlock()
		return row, nil
	}
	_ = p.Pool().NoteSuccess(account, resolvedModel)
	row["ok"] = true
	row["status"] = http.StatusOK
	row["elapsed_ms"] = time.Since(started).Milliseconds()
	if reply == "" {
		reply = "OK"
	}
	if len([]rune(reply)) > 80 {
		reply = string([]rune(reply)[:77]) + "..."
	}
	row["reply"] = reply
	account.mu.RLock()
	row["fails"] = account.State.ErrCount
	account.mu.RUnlock()
	return row, nil
}

func aggregateProbeReply(stream providers.ChatStream) (string, error) {
	var builder strings.Builder
	for i := 0; i < 256; i++ {
		frame, err := stream.Next()
		if len(frame) > 0 {
			data := strings.TrimSpace(streamSSEData(frame))
			if data == "[DONE]" {
				return strings.TrimSpace(builder.String()), nil
			}
			var payload map[string]any
			if json.Unmarshal([]byte(data), &payload) == nil {
				if choices, ok := anyList(payload["choices"]); ok {
					for _, choice := range choices {
						object := creditObject(choice)
						message := creditObject(object["message"])
						delta := creditObject(object["delta"])
						for _, part := range []any{message["content"], message["reasoning_content"], delta["content"], delta["reasoning_content"]} {
							if text, _ := part.(string); text != "" {
								builder.WriteString(text)
							}
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return strings.TrimSpace(builder.String()), nil
			}
			return strings.TrimSpace(builder.String()), err
		}
	}
	return strings.TrimSpace(builder.String()), nil
}
func (p *Provider) handleAccountsEnable(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	payload["enabled"] = true
	p.handleAccountPatch(w, r, payload, "")
}

func (p *Provider) handleAccountsDisable(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	payload["enabled"] = false
	p.handleAccountPatch(w, r, payload, "")
}

func (p *Provider) handleAccountsNote(w http.ResponseWriter, r *http.Request) {
	p.handleSimpleAccountAction(w, r, "note")
}

func (p *Provider) handleAccountsTasks(w http.ResponseWriter, r *http.Request) {
	p.handleSimpleAccountAction(w, r, "tasksEnabled")
}

func (p *Provider) handleAccountsWeight(w http.ResponseWriter, r *http.Request) {
	p.handleSimpleAccountAction(w, r, "weight")
}

func (p *Provider) handleAccountsGroup(w http.ResponseWriter, r *http.Request) {
	p.handleSimpleAccountAction(w, r, "group")
}

func (p *Provider) handleAccountsClearCooldown(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	uid := wbAdminText(payload["uid"])
	if uid == "" {
		writeWBAdminError(w, wbBadRequest("uid required"))
		return
	}
	provider, account := p.providerForUID(uid, wbRequestRealm(r, payload, ""))
	if account == nil {
		writeWBAdminError(w, &wbAdminError{Status: http.StatusNotFound, Type: "invalid_request_error", Message: "no such account"})
		return
	}
	if err := provider.Pool().ClearCooldown(uid); err != nil {
		writeWBAdminError(w, wbAdminErrorFrom(err))
		return
	}
	provider.invalidateHealth()
	writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已清冷却 " + uid})
}

func (p *Provider) handleSimpleAccountAction(w http.ResponseWriter, r *http.Request, field string) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	if field == "tasksEnabled" {
		if raw, present := payload["enabled"]; present {
			payload["tasksEnabled"] = raw
			delete(payload, "enabled")
		}
	}
	p.handleAccountPatch(w, r, payload, "")
}

func (p *Provider) handleSettings(w http.ResponseWriter, r *http.Request) {
	writeWBAdminJSON(w, http.StatusOK, p.settingsView())
}

func (p *Provider) handleSettingsRestrictionReset(w http.ResponseWriter, r *http.Request) {
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"message":           "v2 has no cross-account restriction-hold state; per-account restriction windows are unchanged",
		"restriction_holds": map[string]any{},
	})
}

func (p *Provider) activeRealm() string {
	if p != nil {
		if data, err := os.ReadFile(filepath.Join(p.AccountsDir(), "active_realm.json")); err == nil {
			var payload map[string]any
			if json.Unmarshal(data, &payload) == nil {
				if normalized, ok := normalizeRealm(wbAdminText(payload["realm"])); ok {
					return normalized
				}
			}
		}
		if settings := p.settingsSnapshot(); settings != nil {
			if normalized, ok := normalizeRealm(wbAdminText(settings["active_realm"])); ok {
				return normalized
			}
		}
	}
	return realmIntl
}

func (p *Provider) saveActiveRealm(realm string) error {
	payload := map[string]any{
		"realm":       realm,
		"updated_at":  float64(time.Now().UnixNano()) / float64(time.Second),
		"updated_iso": time.Now().Format("2006-01-02 15:04:05"),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(p.AccountsDir(), "active_realm.json")
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func (p *Provider) handleRealm(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"current": p.activeRealm(),
			"options": []string{realmIntl, realmCN},
		})
		return
	}
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	realm := strings.ToLower(strings.TrimSpace(wbAdminText(payload["realm"])))
	normalized, ok := normalizeRealm(realm)
	if !ok {
		writeWBAdminError(w, wbBadRequest("realm must be intl or cn"))
		return
	}
	if p.store != nil {
		if err := p.saveActiveRealm(normalized); err != nil {
			writeWBAdminError(w, wbServerError("workbuddy: save realm failed"))
			return
		}
	}
	for _, provider := range p.allProviders() {
		provider.invalidateHealth()
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok": true, "current": normalized, "persisted": true,
	})
}

func (p *Provider) settingsView() map[string]any {
	p.refreshSettings()
	settings := map[string]any{}
	if p.store != nil {
		if loaded, err := p.store.Load(); err == nil {
			settings = loaded
		}
	}
	workbuddy := map[string]any{}
	for _, realm := range []string{realmIntl, realmCN} {
		provider := p.providerForRealm(realm)
		if provider == nil {
			continue
		}
		cfg := provider.refreshSettings()
		workbuddy[realm] = configValues(cfg)
	}
	slots := transportSlotsForSettings(settings)
	intlCfg := p.refreshSettings()
	versions := versionsValues(intlCfg.IdentityVersions)
	view := map[string]any{
		"ok":                     true,
		"workbuddy":              workbuddy,
		"schedule":               p.scheduleView(),
		"daily_activity":         p.dailyActivityEnabled(),
		"daily_chat_web":         p.dailyChatWebEnabled(),
		"proxySlots":             slots,
		"auto_switch_product":    intlCfg.AutoSwitchProduct,
		"auto_switch_products":   append([]string(nil), intlCfg.AutoSwitchProducts...),
		"identity_versions":      versions,
		"identity":               map[string]any{"private_data": intlCfg.PrivateData, "versions": versions},
		"account_max_concurrent": intlCfg.AccountMaxConcurrent,
		"realm_fallback_enabled": intlCfg.RealmFallbackEnabled,
		"upstream": map[string]any{
			"header_timeout_seconds": intlCfg.TimeoutSeconds,
			"idle_timeout_seconds":   intlCfg.StreamTimeoutSeconds,
		},
	}
	return view
}

func (p *Provider) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	patch := map[string]any{}
	recognized := false
	if raw, present := payload["schedule"]; present {
		schedule, ok := raw.(map[string]any)
		if !ok {
			writeWBAdminError(w, wbBadRequest("schedule must be an object"))
			return
		}
		normalized := map[string]any{}
		for _, key := range []string{"checkin", "travel", "keepalive", "cat"} {
			if value, exists := schedule[key]; exists {
				hours := normalizeScheduleHours(value)
				if len(hours) == 0 {
					writeWBAdminError(w, wbBadRequest("schedule."+key+" must contain hours 0-23"))
					return
				}
				normalized[key] = hours
			}
		}
		if value, exists := schedule["daily_chat"]; exists {
			times := normalizeScheduleTimes(value)
			if len(times) == 0 && wbAdminText(value) != "" {
				if list, ok := value.([]any); !ok || len(list) > 0 {
					writeWBAdminError(w, wbBadRequest("schedule.daily_chat must be HH:MM times"))
					return
				}
			}
			normalized["daily_chat"] = times
		}
		if len(normalized) > 0 {
			patch["schedule"] = normalized
			recognized = true
		}
	}
	if raw, present := payload["workbuddy"]; present {
		workbuddy, ok := raw.(map[string]any)
		if !ok {
			writeWBAdminError(w, wbBadRequest("workbuddy must be an object"))
			return
		}
		saved := false
		for realm, value := range workbuddy {
			normalized, ok := normalizeRealm(realm)
			if !ok {
				writeWBAdminError(w, wbBadRequest("workbuddy realm must be intl or cn"))
				return
			}
			realmPatch, ok := value.(map[string]any)
			if !ok {
				writeWBAdminError(w, wbBadRequest("workbuddy."+normalized+" must be an object"))
				return
			}
			if _, err := SaveConfig(p.store, normalized, realmPatch); err != nil {
				writeWBAdminError(w, wbBadRequest(err.Error()))
				return
			}
			saved = true
		}
		if saved {
			recognized = true
		}
	}
	// v1 exposes the global WorkBuddy identity/upstream switches at the top
	// level. Accept the same names and project them onto both realm configs.
	globalPatch := map[string]any{}
	for _, key := range []string{
		"auto_switch_product", "auto_switch_products", "identity_versions",
		"account_max_concurrent", "realm_fallback_enabled",
	} {
		if value, present := payload[key]; present {
			globalPatch[key] = value
		}
	}
	if raw, present := payload["upstream"]; present {
		upstream, ok := raw.(map[string]any)
		if !ok {
			writeWBAdminError(w, wbBadRequest("upstream must be an object"))
			return
		}
		if value, exists := upstream["header_timeout_seconds"]; exists {
			globalPatch["timeout_seconds"] = value
		}
		if value, exists := upstream["idle_timeout_seconds"]; exists {
			globalPatch["stream_timeout_seconds"] = value
		}
	}
	if raw, present := payload["identity"]; present {
		identity, ok := raw.(map[string]any)
		if !ok {
			writeWBAdminError(w, wbBadRequest("identity must be an object"))
			return
		}
		if value, exists := identity["private_data"]; exists {
			globalPatch["private_data"] = value
		}
	}
	for _, key := range []string{"timeout_seconds", "stream_timeout_seconds"} {
		if value, present := payload[key]; present {
			globalPatch[key] = value
		}
	}
	if len(globalPatch) > 0 {
		for _, realm := range []string{realmIntl, realmCN} {
			if _, err := SaveConfig(p.store, realm, globalPatch); err != nil {
				writeWBAdminError(w, wbBadRequest(err.Error()))
				return
			}
		}
		recognized = true
	}
	if raw, present := payload["daily_activity"]; present {
		value, ok := wbAdminBool(raw)
		if !ok {
			writeWBAdminError(w, wbBadRequest("daily_activity must be true or false"))
			return
		}
		patch["daily_activity"] = value
		recognized = true
	}
	if raw, present := payload["daily_chat_web"]; present {
		value, ok := wbAdminBool(raw)
		if !ok {
			writeWBAdminError(w, wbBadRequest("daily_chat_web must be true or false"))
			return
		}
		patch["daily_chat_web"] = value
		recognized = true
	}
	if !recognized {
		writeWBAdminError(w, wbBadRequest("nothing to update: pass workbuddy, schedule, daily_activity or daily_chat_web"))
		return
	}
	if p.store != nil {
		if _, err := p.store.Merge(patch); err != nil {
			writeWBAdminError(w, wbServerError("workbuddy: save settings failed"))
			return
		}
	}
	for _, provider := range p.allProviders() {
		provider.refreshSettings()
		provider.invalidateHealth()
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": p.settingsView()})
}

func transportSlotsForSettings(settings map[string]any) []map[string]any {
	slots := transport.SlotsFromSettings(settings)
	out := make([]map[string]any, 0, len(slots))
	for _, slot := range slots {
		out = append(out, map[string]any{
			"id": slot.ID, "name": slot.Name, "enabled": slot.Enabled,
		})
	}
	return out
}

func wbMinInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func accountRealmOf(account *Account) string {
	if account == nil {
		return ""
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return account.Auth.Realm
}

func accountNicknameOf(account *Account) string {
	if account == nil {
		return ""
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return account.Auth.Nickname
}

func accountGroupOf(account *Account) string {
	if account == nil {
		return ""
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return account.Group
}

func accountEnabledForTest(account *Account) bool {
	if account == nil {
		return false
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return account.Auth.Enabled && !account.State.Disabled
}

// invalidateGrowthSnapshot drops the panel's cached growth summary after an
// action that changes task/travel/streak/Buddy state. The next /tasks read then
// reflects the new state instead of the pre-action snapshot.
func (p *Provider) invalidateGrowthSnapshot(uid string) {
	uid = strings.TrimSpace(uid)
	if p == nil || uid == "" || p.Pool() == nil {
		return
	}
	_ = p.Pool().mutateAccount(uid, func(account *Account) {
		if account.Auth.Extra != nil {
			delete(account.Auth.Extra, "growthTasks")
		}
	})
}
