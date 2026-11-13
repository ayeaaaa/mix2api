package raccoon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRaccoonSettingsContributor(t *testing.T) {
	fixture := newRoutesFixture(t)
	view, err := fixture.p.SettingsView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	section, ok := view["raccoon"].(map[string]any)
	if !ok {
		t.Fatalf("view = %#v", view)
	}
	if section["configured"] != false {
		t.Fatalf("disabled section = %#v", section)
	}
	encoded, _ := json.Marshal(section)
	for _, secret := range []string{"access_token", "refresh_token", "office_identity"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("settings leaked %q: %s", secret, encoded)
		}
	}

	if _, err := fixture.store.Merge(map[string]any{
		"raccoon": map[string]any{"future_key": map[string]any{"keep": true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.p.SaveSettings(context.Background(), map[string]any{
		"raccoon": map[string]any{"enabled": true, "timeout_seconds": 30, "max_concurrent": 2},
		"other":   map[string]any{"ignored": true},
	}); err != nil {
		t.Fatal(err)
	}
	stored, _ := fixture.store.Load()
	section, _ = stored["raccoon"].(map[string]any)
	if section["enabled"] != true || fmt.Sprint(section["timeout_seconds"]) != "30" || fmt.Sprint(section["max_concurrent"]) != "2" {
		t.Fatalf("saved section = %#v", section)
	}
	future, _ := section["future_key"].(map[string]any)
	if future["keep"] != true {
		t.Fatalf("unknown nested key lost: %#v", section)
	}
	if err := fixture.p.SaveSettings(context.Background(), map[string]any{"raccoon": "bad"}); err == nil {
		t.Fatal("non-object section accepted")
	}
}
