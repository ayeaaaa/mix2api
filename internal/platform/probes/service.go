package probes

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// ErrNoProvider is returned when no provider implements ProbeProvider.
var ErrNoProvider = errors.New("probes: provider probe hook unavailable")

// Service drives both probe schedulers.
type Service struct {
	// Provider is the legacy single-provider hook. Providers is the composed
	// multi-realm list; when set it wins and Provider remains a fallback for
	// embedders/tests.
	Provider    providers.ProbeProvider
	Providers   []providers.ProbeProvider
	Window      *WindowState
	Quota       *QuotaState
	WatchModels func() []string
	Enabled     func() bool
	Now         func() time.Time
	Sleep       func(time.Duration)
	Logger      func(string)
	Gap         time.Duration
}

// WindowScheduler and QuotaScheduler are the two public scheduler facades.
type WindowScheduler = Service
type QuotaScheduler = Service

// NewService builds the scheduler bundle.
func NewService(opts Service) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	if opts.Gap <= 0 {
		opts.Gap = WindowDefaultGap
	}
	if opts.Window == nil {
		opts.Window = NewWindowState("")
	}
	if opts.Window.Now == nil {
		opts.Window.Now = opts.Now
	}
	if opts.Quota == nil {
		opts.Quota = NewQuotaState("")
	}
	if opts.Quota.Now == nil {
		opts.Quota.Now = opts.Now
	}
	return &opts
}

// NewWindowScheduler returns the same service as a window facade.
func NewWindowScheduler(service *Service) *WindowScheduler { return service }

// NewQuotaScheduler returns the same service as a quota facade.
func NewQuotaScheduler(service *Service) *QuotaScheduler { return service }

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) enabled() bool {
	if s != nil && s.Enabled != nil {
		return s.Enabled()
	}
	return true
}

func (s *Service) log(message string) {
	if s != nil && s.Logger != nil {
		s.Logger(message)
	}
}

func (s *Service) sleep(duration time.Duration) {
	if duration > 0 && s != nil && s.Sleep != nil {
		s.Sleep(duration)
	}
}

// RunWindow fires due model-window probes. A missing provider is a no-op.
func (s *Service) RunWindow(ctx context.Context) int {
	if s == nil || len(s.providerList()) == 0 || !s.enabled() {
		return 0
	}
	now := s.now()
	due := s.Window.Due(now, WindowBatchLimit)
	sent := 0
	for _, window := range due {
		target := providers.ProbeTarget{UID: window.UID, Model: window.Model, Realm: window.Realm, ResetAt: time.Unix(int64(window.ResetAt), 0)}
		provider := s.providerForTarget(ctx, providers.ProbeWindow, target)
		if provider == nil {
			continue
		}
		sent++
		result, err := provider.Probe(ctx, providers.ProbeWindow, target)
		if err != nil {
			s.log("窗口探针执行失败: " + err.Error())
			s.retryWindow(window, now, err.Error())
			s.sleep(s.Gap)
			continue
		}
		if result.OK {
			s.Window.DropRealm(window.Realm, window.UID, window.Model)
			s.log("✓ 窗口探针成功: " + first8(window.UID) + " " + window.Model)
		} else if result.ResetAt.After(now.Add(WindowFreshThreshold)) {
			s.Window.Update(window, result.ResetAt, 0, result.Detail, now)
		} else {
			s.retryWindow(window, now, result.Detail)
		}
		s.sleep(s.Gap)
	}
	return sent
}

func (s *Service) retryWindow(window Window, now time.Time, detail string) {
	attempts := window.Attempts + 1
	if attempts >= WindowMaxAttempts {
		s.Window.DropRealm(window.Realm, window.UID, window.Model)
		s.log("窗口探针放弃: " + first8(window.UID) + " " + window.Model)
		return
	}
	s.Window.Update(window, now.Add(WindowRetryDelay), attempts, detail, now)
}

// RunQuota fires due quota-scan targets. force ignores the interval but never
// bypasses an existing 429 window.
func (s *Service) RunQuota(ctx context.Context, force bool) (int, error) {
	if s == nil || len(s.providerList()) == 0 {
		return 0, ErrNoProvider
	}
	if !s.enabled() {
		return 0, nil
	}
	watched := s.watchModels()
	if len(watched) == 0 {
		return 0, nil
	}
	watchedSet := map[string]bool{}
	for _, model := range watched {
		watchedSet[model] = true
	}
	accounts, err := s.allTargets(ctx, providers.ProbeQuota, 0)
	if err != nil {
		return 0, err
	}
	now := s.now()
	// Seed all watched account/model lines with the v1 staggered first delay.
	wanted := []struct{ uid, model, realm string }{}
	for _, target := range accounts {
		if target.UID == "" {
			continue
		}
		realm := target.Realm
		if realm == "" {
			realm = s.realmOfProvider(s.providerForTarget(ctx, providers.ProbeQuota, target))
		}
		if target.Model != "" {
			if !watchedSet[target.Model] {
				continue
			}
			s.Quota.EnsureRealm(target.UID, target.Model, realm, now, QuotaFirstDelay)
			continue
		}
		for _, model := range watched {
			wanted = append(wanted, struct{ uid, model, realm string }{target.UID, model, realm})
		}
	}
	sort.Slice(wanted, func(i, j int) bool {
		if wanted[i].uid != wanted[j].uid {
			return wanted[i].uid < wanted[j].uid
		}
		return wanted[i].model < wanted[j].model
	})
	delay := QuotaFirstDelay
	for _, entry := range wanted {
		if s.Quota.EnsureRealm(entry.uid, entry.model, entry.realm, now, delay) {
			delay += QuotaFirstStagger
			if delay > QuotaFirstDelayMax {
				delay = QuotaFirstDelayMax
			}
		}
	}
	due := s.Quota.Due(watchedSet, now, QuotaBatchLimit, force)
	sent := 0
	for _, row := range due {
		if s.Window.HasWindowRealm(row.Realm, row.UID, row.Model) {
			continue
		}
		target := providers.ProbeTarget{UID: row.UID, Model: row.Model, Realm: row.Realm}
		provider := s.providerForTarget(ctx, providers.ProbeQuota, target)
		if provider == nil {
			continue
		}
		sent++
		result, probeErr := provider.Probe(ctx, providers.ProbeQuota, target)
		if probeErr != nil {
			s.quotaFailure(row, now, probeErr.Error(), false)
			s.sleep(s.Gap)
			continue
		}
		switch {
		case result.OK:
			s.Window.DropRealm(row.Realm, row.UID, row.Model)
			s.Quota.RecordRealm(row.Realm, row.UID, row.Model, now, true, time.Time{}, 0, "ok", "")
			s.log("✓ 额度查询正常: " + first8(row.UID) + " " + row.Model)
		case result.ResetAt.After(now.Add(5 * time.Second)):
			s.Window.ScheduleRealm(row.UID, row.Model, row.Realm, result.ResetAt, result.Status == 429, now, now)
			s.Quota.RecordRealm(row.Realm, row.UID, row.Model, now, true, time.Time{}, 0, "throttled", result.Detail)
			s.log("额度查询已满: " + first8(row.UID) + " " + row.Model)
		default:
			detail := result.Detail
			if result.Status == 429 {
				s.quotaFailure(row, now, detail, true)
			} else {
				s.quotaFailure(row, now, detail, false)
			}
		}
		s.sleep(s.Gap)
	}
	return sent, nil
}

func (s *Service) quotaFailure(row QuotaRow, now time.Time, detail string, throttled bool) {
	attempts := row.Attempts + 1
	result := "error"
	if throttled {
		result = "throttled"
	}
	if attempts >= QuotaMaxAttempts {
		s.Quota.RecordRealm(row.Realm, row.UID, row.Model, now, true, time.Time{}, 0, result, detail)
		return
	}
	s.Quota.RecordRealm(row.Realm, row.UID, row.Model, now, false, now.Add(QuotaRetryDelay), attempts, result, detail)
}

func (s *Service) watchModels() []string {
	if s == nil || s.WatchModels == nil {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, model := range s.WatchModels() {
		if value := sanitizeModel(model); value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func sanitizeModel(value string) string {
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t' || value[0] == '\n' || value[0] == '\r') {
		value = value[1:]
	}
	for len(value) > 0 {
		last := value[len(value)-1]
		if last == ' ' || last == '\t' || last == '\n' || last == '\r' {
			value = value[:len(value)-1]
			continue
		}
		break
	}
	return value
}

func splitKey(key string) [2]string {
	for index := 0; index < len(key); index++ {
		if key[index] == 0 {
			return [2]string{key[:index], key[index+1:]}
		}
	}
	return [2]string{key, ""}
}

// WindowSnapshot returns the panel window rows.
func (s *Service) WindowSnapshot() []map[string]any {
	if s == nil || s.Window == nil {
		return []map[string]any{}
	}
	return s.Window.Snapshot(s.now())
}

// QuotaSnapshot returns the panel quota rows.
func (s *Service) QuotaSnapshot() []map[string]any {
	if s == nil || s.Quota == nil {
		return []map[string]any{}
	}
	watched := map[string]bool{}
	for _, model := range s.watchModels() {
		watched[model] = true
	}
	return s.Quota.Snapshot(watched, s.now(), 20)
}

func (s *Service) providerList() []providers.ProbeProvider {
	if s == nil {
		return nil
	}
	if len(s.Providers) > 0 {
		return s.Providers
	}
	if s.Provider != nil {
		return []providers.ProbeProvider{s.Provider}
	}
	return nil
}

func probeProviderRealm(provider providers.ProbeProvider) string {
	if realmProvider, ok := provider.(interface{ Realm() string }); ok {
		return strings.TrimSpace(realmProvider.Realm())
	}
	return ""
}

func (s *Service) realmOfProvider(provider providers.ProbeProvider) string {
	return probeProviderRealm(provider)
}

func (s *Service) providerForTarget(ctx context.Context, kind providers.ProbeKind, target providers.ProbeTarget) providers.ProbeProvider {
	if realm := strings.TrimSpace(target.Realm); realm != "" {
		for _, provider := range s.providerList() {
			if strings.EqualFold(probeProviderRealm(provider), realm) {
				return provider
			}
		}
	}
	for _, provider := range s.providerList() {
		targets, err := provider.ProbeTargets(ctx, kind, s.now(), 0)
		if err != nil {
			continue
		}
		for _, candidate := range targets {
			if candidate.UID == target.UID {
				return provider
			}
		}
	}
	if providers := s.providerList(); len(providers) == 1 {
		return providers[0]
	}
	return nil
}

func (s *Service) allTargets(ctx context.Context, kind providers.ProbeKind, limit int) ([]providers.ProbeTarget, error) {
	out := []providers.ProbeTarget{}
	seen := map[string]bool{}
	for _, provider := range s.providerList() {
		realm := probeProviderRealm(provider)
		targets, err := provider.ProbeTargets(ctx, kind, s.now(), limit)
		if err != nil {
			return nil, err
		}
		for _, target := range targets {
			if target.Realm == "" {
				target.Realm = realm
			}
			key := target.Realm + "\x00" + target.UID + "\x00" + target.Model
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, target)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}
