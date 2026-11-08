package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// creditInfoTTL mirrors v1's 60-second account-info cache.
	creditInfoTTL = 60 * time.Second

	// creditInfoTimeout and checkinTimeout mirror v1's default request budgets.
	creditInfoTimeout = 15 * time.Second
	checkinTimeout    = 30 * time.Second

	// creditMaxBodyBytes bounds every Task 7 response body.
	creditMaxBodyBytes = 1 << 20

	// creditErrorMessageRunes and creditRefreshErrorRunes mirror v1's display
	// caps: 300 for a panel message, 160 for each refresh aggregate entry.
	creditErrorMessageRunes   = 300
	creditRefreshErrorRunes   = 160
	creditRefreshErrorLimit   = 3
	creditEndpointPlugin      = "/v1/statistics/plugin"
	creditEndpointDelivery    = "/v1/ops/delivery"
	creditEndpointClaim       = "/v1/ops/claim"
	creditEndpointConfirm     = "/v1/ops/confirm"
	creditCheckinChannel      = "IDE"
	creditCheckinAgentType    = "PromptCenter"
	creditCheckinLanguage     = "zh-cn"
	creditCheckinBusyMessage  = "该账号签到正在进行中，稍后刷新即可"
	creditNoCampaignsMessage  = "无可领取的签到或新手活动"
	creditTotalPackageMetric  = "usageTotalPackageCredit"
	creditBasicPackageMetric  = "usageBasicPackageCredit"
	creditOnDemandMetric      = "usageOnDemandPackageCredit"
	creditBonusPackageMetric  = "usageBonusPackageCredit"
	creditRequiredUnit        = "CREDIT"
	creditConfirmedStatus     = "CONFIRMED"
	creditConsumedStatus      = "CONSUMED"
	creditEligibleStatus      = "ELIGIBLE"
	creditClaimedStatus       = "CLAIMED"
	creditSuccessStatus       = "CONFIRMED"
	creditCampaignFallbackFmt = "积分活动 %s"
)

// CreditsRefreshRow is one per-account row from RefreshCredits. A failed row
// carries a redacted message and no public credits view.
type CreditsRefreshRow struct {
	OK      bool         `json:"ok"`
	UID     string       `json:"uid"`
	Credits *CreditsView `json:"credits,omitempty"`
	Message string       `json:"message"`
}

// CreditsRefreshResult is the v1-compatible refresh envelope. Refreshed counts
// successful accounts; Errors contains up to v1's three redacted failure
// strings. A single failed account never makes the whole call fail.
type CreditsRefreshResult struct {
	OK        bool                `json:"ok"`
	Refreshed int                 `json:"refreshed"`
	Errors    []string            `json:"errors"`
	Message   string              `json:"message"`
	Accounts  []CreditsRefreshRow `json:"accounts"`
}

// FetchInfo refreshes one account's package/credit/statistics snapshot. It
// returns the cached snapshot for 60 seconds unless force is set. A failed
// refresh returns the last good snapshot alongside the error so callers can
// still render the panel; a cold failure returns no snapshot.
func (p *Provider) FetchInfo(ctx context.Context, a *Account, force bool) (*AccountInfo, error) {
	if p == nil {
		return nil, errors.New("codearts: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("codearts: account info requires a context")
	}
	if a == nil {
		return nil, errors.New("codearts: account info requires an account")
	}
	if !force {
		if cached, ok := a.freshInfo(p.catalogNow(), creditInfoTTL); ok {
			return cached, nil
		}
	}

	a.infoMu.Lock()
	defer a.infoMu.Unlock()
	if !force {
		// Another caller may have refreshed while this one waited.
		if cached, ok := a.freshInfo(p.catalogNow(), creditInfoTTL); ok {
			return cached, nil
		}
	}

	// Fetching statistics only needs the current signed credential pair. A
	// refresh failure must not hide a still-usable read-only request.
	if p.tokenManager != nil {
		_ = p.tokenManager.Ensure(ctx, a)
	}

	signed := http.Header{}
	signed.Set("Content-Type", "application/json")
	afterSign := http.Header{}
	afterSign.Set("Agent-Type", creditCheckinAgentType)
	afterSign.Set("X-Language", creditCheckinLanguage)
	data, err := p.signedJSON(ctx, a, signedJSONRequest{
		Method:    http.MethodGet,
		URL:       snapManagerBase() + creditEndpointPlugin,
		Timeout:   creditInfoTimeout,
		Signed:    signed,
		AfterSign: afterSign,
	})
	if err != nil {
		if prior := a.infoSnapshot(); prior != nil {
			return prior, err
		}
		return nil, err
	}

	info := parseAccountInfo(data, p.catalogNow())
	a.SetInfo(info)
	return a.infoSnapshot(), nil
}

// parseAccountInfo normalizes the statistics/plugin response into the v1
// panel shape. Missing metrics become nil buckets; present zero buckets stay
// present so the panel can distinguish "0 used" from "not reported".
func parseAccountInfo(data map[string]any, now time.Time) *AccountInfo {
	packageInfo := modelMap(data["package"])
	isCredit, _ := packageInfo["is_credit_package"].(bool)
	isToken, _ := packageInfo["is_token_package"].(bool)
	packageName := strings.TrimSpace(modelTruthyText(packageInfo["package_name_cn"]))
	if packageName == "" {
		packageName = strings.TrimSpace(modelTruthyText(packageInfo["package_name_en"]))
	}

	metrics := data["metrics"]
	models := make([]ModelInfo, 0)
	for _, raw := range modelList(data["model_metrics"]) {
		item := modelMap(raw)
		if item == nil {
			continue
		}
		id := strings.TrimSpace(modelTruthyText(item["model_id"]))
		if id == "" {
			continue
		}
		name := strings.TrimSpace(modelTruthyText(item["model_name"]))
		if name == "" {
			name = id
		}
		enabled := true
		if value, ok := item["enable"].(bool); ok && !value {
			enabled = false
		}
		models = append(models, ModelInfo{
			ID:          id,
			Name:        name,
			Description: modelTruthyText(item["model_desc"]),
			Type:        modelTruthyText(item["model_type"]),
			Enabled:     enabled,
			Status:      modelTruthyText(item["status"]),
			Luxury:      modelTruthy(item["is_luxury"]),
		})
	}

	return &AccountInfo{
		IsCreditPackage: isCredit,
		IsTokenPackage:  isToken,
		SpecCode:        modelTruthyText(packageInfo["spec_code"]),
		PackageName:     packageName,
		PackageStatus:   modelTruthyText(packageInfo["status"]),
		StartDate:       modelTruthyText(data["start_date"]),
		EndDate:         modelTruthyText(data["end_date"]),
		Credits: AccountCredits{
			Total:    creditMetric(metrics, creditTotalPackageMetric),
			Basic:    creditMetric(metrics, creditBasicPackageMetric),
			OnDemand: creditMetric(metrics, creditOnDemandMetric),
			Bonus:    creditMetric(metrics, creditBonusPackageMetric),
			Unit:     "credit",
		},
		Models:    models,
		FetchedAt: unixSeconds(now),
	}
}

// creditMetric finds one named metric and converts all four v1 numeric fields.
// The first matching row wins, matching the Python helper's linear scan.
func creditMetric(metrics any, name string) *CreditTotal {
	for _, raw := range modelList(metrics) {
		item := modelMap(raw)
		if item == nil || modelText(item["name"]) != name {
			continue
		}
		return &CreditTotal{
			Amount:   creditNumber(item["package_credit_amount"]),
			Used:     creditNumber(item["package_credit_used"]),
			Remain:   creditNumber(item["package_credit_remain"]),
			Expiring: creditNumber(item["package_credit_expiring_amount"]),
		}
	}
	return nil
}

func creditNumber(value any) float64 {
	number, ok := numericValue(value)
	if !ok {
		return 0
	}
	return number
}

// RefreshCredits force-refreshes every enabled, live account in v1's
// deterministic UID order. Disabled/manual-disabled accounts are skipped,
// matching v1's refresh loop. Per-account failures are returned as redacted
// rows and the first three are also listed in Errors; the call itself succeeds
// so one bad account cannot hide the rest of the pool.
func (p *Provider) RefreshCredits(ctx context.Context) (CreditsRefreshResult, error) {
	if p == nil {
		return CreditsRefreshResult{}, errors.New("codearts: provider is nil")
	}
	if p.pool == nil {
		return CreditsRefreshResult{}, errors.New("codearts: account pool is nil")
	}
	if ctx == nil {
		return CreditsRefreshResult{}, errors.New("codearts: credits refresh requires a context")
	}

	result := CreditsRefreshResult{
		OK:       true,
		Errors:   make([]string, 0),
		Accounts: make([]CreditsRefreshRow, 0),
	}
	for _, account := range p.pool.Accounts() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		uid, live := account.currentUID()
		if !live {
			continue
		}
		nickname, disabled := creditsAccountLabels(account)
		if disabled {
			continue
		}

		row := CreditsRefreshRow{OK: true, UID: uid}
		if p.tokenManager != nil {
			if err := p.tokenManager.Ensure(ctx, account); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return result, ctxErr
				}
				row.OK = false
				row.Message = redactedCreditsError(err, creditsAccountAuth(account), creditErrorMessageRunes)
				result.addFailure(row, nickname)
				continue
			}
		}
		info, err := p.FetchInfo(ctx, account, true)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			row.OK = false
			row.Message = redactedCreditsError(err, creditsAccountAuth(account), creditErrorMessageRunes)
			result.addFailure(row, nickname)
			continue
		}
		if info != nil {
			row.Credits = publicCredits(info)
		}
		result.Refreshed++
		result.Accounts = append(result.Accounts, row)
	}

	result.Message = fmt.Sprintf("已刷新 %d 个账号的积分", result.Refreshed)
	return result, nil
}

func (result *CreditsRefreshResult) addFailure(row CreditsRefreshRow, nickname string) {
	result.OK = false
	result.Accounts = append(result.Accounts, row)
	if len(result.Errors) >= creditRefreshErrorLimit {
		return
	}
	message := row.Message
	if message == "" {
		message = "unknown error"
	}
	entry := nickname + ": " + redactDisplayText(message, creditRefreshErrorRunes)
	result.Errors = append(result.Errors, entry)
}

func creditsAccountLabels(account *Account) (nickname string, disabled bool) {
	if account == nil {
		return "", true
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	nickname = strings.TrimSpace(account.Auth.UserName)
	if nickname == "" {
		nickname = account.uidLocked()
	}
	return nickname, account.State.Disabled || account.State.ManualDisabled
}

func creditsAccountAuth(account *Account) Auth {
	if account == nil {
		return Auth{}
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.Auth
}

// signedJSONRequest describes one Huawei-signed JSON request. Headers in Signed
// participate in the SDK-HMAC canonical request; headers in AfterSign are
// appended only after signing, matching v1's statistics endpoint requirement.
type signedJSONRequest struct {
	Method    string
	URL       string
	Payload   map[string]any
	Timeout   time.Duration
	Signed    http.Header
	AfterSign http.Header
}

// signedJSON sends one signed request through the shared transport, decodes a
// JSON object with numbers preserved, bounds the response body, and redacts
// every known credential from errors.
func (p *Provider) signedJSON(ctx context.Context, a *Account, options signedJSONRequest) (map[string]any, error) {
	if p == nil || p.transport == nil {
		return nil, errors.New("codearts: signed request transport is nil")
	}
	if ctx == nil {
		return nil, errors.New("codearts: signed request requires a context")
	}
	if a == nil {
		return nil, errors.New("codearts: signed request requires an account")
	}

	a.mu.Lock()
	auth := a.Auth
	proxyURL := a.proxy
	removed := a.removed
	a.mu.Unlock()
	if removed {
		return nil, ErrAccountNotFound
	}

	var body []byte
	if options.Payload != nil {
		encoded, err := json.Marshal(options.Payload)
		if err != nil {
			return nil, errors.New("codearts: encode signed request body failed")
		}
		body = encoded
	}

	requestCtx := ctx
	if options.Timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, options.Timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestCtx, options.Method, options.URL, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("codearts: build signed request failed")
	}
	for key, values := range options.Signed {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	credentials := SignCredentials{
		AccessKeyID:     auth.AccessKeyID,
		SecretAccessKey: auth.SecretAccessKey,
		SecurityToken:   auth.CloudDragonToken,
	}
	if err := SignRequest(request, body, credentials); err != nil {
		return nil, errors.New("codearts: sign request failed")
	}
	for key, values := range options.AfterSign {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	response, err := p.transport.Do(requestCtx, request, proxyURL)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("codearts: signed request transport failed: %s",
			catalogSafeError(err.Error(), auth))
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, creditMaxBodyBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("codearts: read signed response failed: %s",
			catalogSafeError(err.Error(), auth))
	}
	if len(raw) > creditMaxBodyBytes {
		return nil, fmt.Errorf("codearts: signed response exceeded %d bytes", creditMaxBodyBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("codearts: signed request HTTP %d: %s",
			response.StatusCode, catalogSafeError(catalogErrorText(raw), auth))
	}

	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil || decoded == nil {
		return nil, fmt.Errorf("codearts: signed request returned a non-JSON response (HTTP %d)",
			response.StatusCode)
	}
	return decoded, nil
}

// redactedCreditsError renders an account failure for a panel row. It caps the
// text and removes known credentials, proxy userinfo, and token-shaped values.
func redactedCreditsError(err error, auth Auth, limit int) string {
	if err == nil {
		return ""
	}
	text := catalogSafeError(err.Error(), auth)
	return redactDisplayText(text, limit)
}

func redactDisplayText(text string, limit int) string {
	text = truncateRunes(strings.TrimSpace(text), limit)
	return text
}
