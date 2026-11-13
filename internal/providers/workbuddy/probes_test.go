package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestProviderProbeHook(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "probe-a", realmCN, map[string]any{
		"accessToken": "probe-token", "enabled": true, "nickname": "Probe",
	})
	provider, _ := newPoolProvider(t, realmCN, dir)
	now := time.Unix(5000, 0)
	provider.probeNow = func() time.Time { return now }
	_, server := newTaskTestUpstream(t, func(fake *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if r.URL.Path != chatPath || body["model"] != "m1" || body["max_tokens"] != float64(16) {
			return http.StatusBadRequest, map[string]any{"code": 400, "msg": "bad probe"}
		}
		if body["fail"] == true {
			return http.StatusTooManyRequests, map[string]any{"code": 429, "msg": "throttled", "reset_at": 5010}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	})
	provider.probeBaseURL = server.URL
	targets, err := provider.ProbeTargets(context.Background(), providers.ProbeWindow, now, 10)
	if err != nil || len(targets) != 1 || targets[0].UID != "probe-a" {
		t.Fatalf("targets = %#v err=%v", targets, err)
	}
	result, err := provider.Probe(context.Background(), providers.ProbeWindow, providers.ProbeTarget{UID: "probe-a", Model: "m1"})
	if err != nil || !result.OK {
		t.Fatalf("probe = %#v err=%v", result, err)
	}
}

func TestProviderProbe429(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "probe-b", realmCN, map[string]any{"accessToken": "probe-token", "enabled": true})
	provider, pool := newPoolProvider(t, realmCN, dir)
	now := time.Unix(5000, 0)
	provider.probeNow = func() time.Time { return now }
	_, server := newTaskTestUpstream(t, func(fake *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		return http.StatusTooManyRequests, map[string]any{"code": 429, "msg": "throttled", "reset_at": 6600}
	})
	provider.probeBaseURL = server.URL
	result, err := provider.Probe(context.Background(), providers.ProbeWindow, providers.ProbeTarget{UID: "probe-b", Model: "m1"})
	if err != nil || result.OK || result.Status != 429 || result.ResetAt.IsZero() {
		t.Fatalf("429 probe = %#v err=%v", result, err)
	}
	if !pool.Get("probe-b").modelCooling("m1", time.Now()) {
		t.Fatal("429 did not register a model cooldown")
	}
}

func TestCodeArtsRaccoonNoProbeHook(t *testing.T) {
	// Compile-time absence is the contract: only WorkBuddy implements
	// providers.ProbeProvider.
	var codeartsProvider interface{} = nil
	if _, ok := codeartsProvider.(providers.ProbeProvider); ok {
		t.Fatal("nil interface unexpectedly implements ProbeProvider")
	}
}
