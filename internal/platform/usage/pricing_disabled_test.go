package usage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPricingDisabledIsNotCostMissing(t *testing.T) {
	now := float64(time.Now().Unix())
	env := newTestEnv(t, []map[string]any{
		row(now, "m", "intl", "a", 100, 10, nil),
		row(now-1, "missing", "intl", "a", 10, 1, nil),
	})
	settings, _ := json.Marshal(map[string]any{"pricing_enabled": false})
	if err := os.WriteFile(filepath.Join(env.Accounts, "settings.json"), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := env.service().Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	missing := snapshot["cost_missing"].(map[string]int64)
	if len(missing) != 0 {
		t.Fatalf("disabled pricing counted as missing: %+v", missing)
	}
	if mustNumber(t, snapshot["cost_cny"]) != 0 {
		t.Fatalf("disabled pricing produced a cost: %+v", snapshot["cost_cny"])
	}
}
