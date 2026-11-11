package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	probeDefaultTimeout = 60 * time.Second
	probePrompt         = "Hi"
	probeMaxTokens      = 16
)

func (p *Provider) probeClient() ContextHTTPDoer {
	if p != nil && p.probeDoer != nil {
		return p.probeDoer
	}
	return p.taskClient()
}

func (p *Provider) probeBase() string {
	if p != nil {
		if base := strings.TrimSpace(p.probeBaseURL); base != "" {
			return strings.TrimRight(base, "/")
		}
		if base := strings.TrimSpace(p.tasksChatBaseURL); base != "" {
			return strings.TrimRight(base, "/")
		}
	}
	if p == nil {
		return ""
	}
	return strings.TrimRight(p.taskChatBase(), "/")
}

func (p *Provider) probeNowTime() time.Time {
	if p != nil && p.probeNow != nil {
		return p.probeNow()
	}
	return time.Now()
}

// ProbeTargets implements providers.ProbeProvider. It returns accounts that
// can be probed; the platform expands these across its watched model list.
func (p *Provider) ProbeTargets(ctx context.Context, kind providers.ProbeKind, now time.Time, limit int) ([]providers.ProbeTarget, error) {
	if p == nil || p.Pool() == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 64
	}
	out := []providers.ProbeTarget{}
	for _, account := range p.Pool().Accounts() {
		if account == nil || !account.Auth.Enabled || strings.TrimSpace(account.Auth.AccessToken) == "" {
			continue
		}
		out = append(out, providers.ProbeTarget{UID: account.UID(), Realm: p.realm})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Probe implements providers.ProbeProvider with the same minimal streaming
// request v1's window probe used.
func (p *Provider) Probe(ctx context.Context, kind providers.ProbeKind, target providers.ProbeTarget) (providers.ProbeResult, error) {
	if p == nil {
		return providers.ProbeResult{Detail: "provider unavailable"}, nil
	}
	if strings.TrimSpace(target.Model) == "" {
		return providers.ProbeResult{Detail: "model is required"}, nil
	}
	account := p.accountByUID(target.UID)
	if account == nil {
		return providers.ProbeResult{Detail: "account unavailable"}, nil
	}
	if !account.Auth.APIKey && strings.TrimSpace(account.Auth.RefreshToken) != "" {
		expiry := account.Auth.ExpiresAt
		if expiry.IsZero() {
			if claims := parseJWTClaims(account.Auth.AccessToken); claims.ExpiresAt > 0 {
				expiry = time.Unix(claims.ExpiresAt, 0)
			}
		}
		if !expiry.IsZero() && p.probeNowTime().Add(tokenRefreshSkew).After(expiry) {
			_, _ = p.ensureToken(ctx, account, tokenRefreshRequestPath)
		}
	}
	upstreamModel := intlModelAlias(target.Model, p.realm)
	body := map[string]any{
		"model": upstreamModel,
		"messages": []any{map[string]any{
			"role": "user", "content": probePrompt,
		}},
		"stream":           true,
		"max_tokens":       probeMaxTokens,
		"reasoning_effort": "none",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return providers.ProbeResult{Detail: "build request failed"}, nil
	}
	client := p.probeClient()
	if client == nil {
		return providers.ProbeResult{Detail: "probe client unavailable"}, nil
	}
	timeout := probeDefaultTimeout
	if p.probeTimeout > 0 {
		timeout = p.probeTimeout
	}
	requestCtx, cancel := context.WithTimeout(nonNilContext(ctx), timeout)
	defer cancel()
	endpoint := p.probeBase() + chatPath
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return providers.ProbeResult{Detail: "probe request invalid"}, nil
	}
	request.Header = p.taskChatHeaders(account, false)
	proxyURL := ""
	account.mu.RLock()
	proxyURL = strings.TrimSpace(account.Auth.ProxyURL)
	account.mu.RUnlock()
	response, err := client.Do(requestCtx, request, proxyURL)
	if err != nil {
		return providers.ProbeResult{Detail: "probe request failed"}, nil
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	status := response.StatusCode
	if status >= 200 && status < 300 {
		_ = p.Pool().NoteSuccess(account, target.Model)
		return providers.ProbeResult{OK: true, Status: status}, nil
	}
	detail := strings.TrimSpace(string(payload))
	if len(detail) > 200 {
		detail = detail[:200]
	}
	result := providers.ProbeResult{Status: status, Detail: detail}
	if status == http.StatusTooManyRequests {
		result.ResetAt = parseChatResetAt(string(payload), p.probeNowTime())
		if !result.ResetAt.IsZero() {
			wait := time.Until(result.ResetAt)
			if wait <= 0 {
				wait = time.Minute
			}
			_ = p.Pool().NoteModelError(account, target.Model, detail, wait)
		}
	}
	return result, nil
}

var _ providers.ProbeProvider = (*Provider)(nil)
var _ = errors.New
