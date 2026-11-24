package settingflags

import "testing"

func TestDefaults(t *testing.T) {
	if !DefaultTrue(nil, "x") || !DefaultTrue(map[string]any{}, "x") || DefaultTrue(map[string]any{"x": false}, "x") {
		t.Fatal("DefaultTrue")
	}
	if DefaultFalse(nil, "x") || DefaultFalse(map[string]any{"x": "true"}, "x") || !DefaultFalse(map[string]any{"x": true}, "x") {
		t.Fatal("DefaultFalse")
	}
}
