package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCodeArtsSettingsContributor(t *testing.T) {
	fixture := newCodeArtsRoutesFixture(t, false)
	view, err := fixture.p.SettingsView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	section, ok := view["codearts"].(map[string]any)
	if !ok {
		t.Fatalf("view = %#v", view)
	}
	if section["configured"] != false || section["accounts"] != 0 {
		t.Fatalf("disabled section = %#v", section)
	}
	encoded, _ := json.Marshal(section)
	for _, secret := range []string{"access_key", "secret_access_key", "cloud_dragon_token", "refresh_token"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("settings leaked %q: %s", secret, encoded)
		}
	}

	if _, err := fixture.store.Merge(map[string]any{
		"codearts": map[string]any{"future_key": map[string]any{"keep": true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.p.SaveSettings(context.Background(), map[string]any{
		"codearts": map[string]any{"enabled": true, "timeout_seconds": 5, "max_concurrent": 3},
		"other":    map[string]any{"ignored": true},
	}); err != nil {
		t.Fatal(err)
	}
	stored, _ := fixture.store.Load()
	section, _ = stored["codearts"].(map[string]any)
	if section["enabled"] != true || testText(section["timeout_seconds"]) != "5" || testText(section["max_concurrent"]) != "3" {
		t.Fatalf("saved section = %#v", section)
	}
	future, _ := section["future_key"].(map[string]any)
	if future["keep"] != true {
		t.Fatalf("unknown nested key lost: %#v", section)
	}
	if err := fixture.p.SaveSettings(context.Background(), map[string]any{"codearts": "bad"}); err == nil {
		t.Fatal("non-object section accepted")
	}
}

func testText(value any) string {
	return fmt.Sprint(value)
}
