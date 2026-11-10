package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SettingsView returns the WorkBuddy-owned panel settings sections. The
// platform settings service merges this map with its base view.
func (p *Provider) SettingsView(ctx context.Context) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("workbuddy: provider is nil")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return p.settingsView(), nil
}

// SaveSettings applies the WorkBuddy-owned parts of a panel patch. It reuses
// the exact HTTP handler body so the panel route and the platform contributor
// cannot drift; only the transport wrapper differs.
func (p *Provider) SaveSettings(ctx context.Context, patch map[string]any) error {
	if p == nil {
		return fmt.Errorf("workbuddy: provider is nil")
	}
	if !p.hasWorkBuddySettingsPatch(patch) {
		return nil
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("workbuddy: encode settings patch: %w", err)
	}
	request, err := http.NewRequestWithContext(requestContext(ctx), http.MethodPost, "/settings/save", bytes.NewReader(body))
	if err != nil {
		return err
	}
	recorder := &settingsResponseRecorder{header: make(http.Header)}
	p.handleSettingsSave(recorder, request)
	if recorder.status >= http.StatusBadRequest {
		message := recorder.errorMessage()
		if message == "" {
			message = "workbuddy: settings save failed"
		}
		return fmt.Errorf("%s", message)
	}
	return nil
}

func requestContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// hasWorkBuddySettingsPatch lets platform-only saves pass through without the
// handler's "nothing to update" error.
func (p *Provider) hasWorkBuddySettingsPatch(patch map[string]any) bool {
	for _, key := range []string{
		"schedule", "workbuddy", "daily_activity", "daily_chat_web",
		"auto_switch_product", "auto_switch_products", "identity_versions",
		"account_max_concurrent", "realm_fallback_enabled", "upstream",
		"identity", "timeout_seconds", "stream_timeout_seconds",
	} {
		if _, present := patch[key]; present {
			return true
		}
	}
	return false
}

type settingsResponseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *settingsResponseRecorder) Header() http.Header { return r.header }

func (r *settingsResponseRecorder) WriteHeader(status int) { r.status = status }

func (r *settingsResponseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(data)
}

func (r *settingsResponseRecorder) errorMessage() string {
	var payload map[string]any
	if err := json.Unmarshal(r.body.Bytes(), &payload); err != nil {
		return strings.TrimSpace(r.body.String())
	}
	errorValue, _ := payload["error"].(map[string]any)
	return strings.TrimSpace(textValue(errorValue["message"]))
}

func textValue(value any) string {
	text, _ := value.(string)
	return text
}

var _ io.Writer = (*settingsResponseRecorder)(nil)

// ActiveRealm returns the persisted WorkBuddy exit.
func (p *Provider) ActiveRealm(ctx context.Context) string {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return realmIntl
		}
	}
	return p.activeRealm()
}

// SetActiveRealm persists the WorkBuddy exit and invalidates provider health.
func (p *Provider) SetActiveRealm(ctx context.Context, realm string) error {
	normalized, ok := normalizeRealm(realm)
	if !ok {
		return fmt.Errorf("realm must be intl or cn")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if p.store != nil {
		if err := p.saveActiveRealm(normalized); err != nil {
			return fmt.Errorf("workbuddy: save realm failed")
		}
	}
	for _, provider := range p.allProviders() {
		provider.invalidateHealth()
	}
	return nil
}

// RealmOptions reports the WorkBuddy exits.
func (p *Provider) RealmOptions() []string {
	return []string{realmIntl, realmCN}
}
