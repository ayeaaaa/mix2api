package api

import "testing"

func TestParseModelRef(t *testing.T) {
	isExit := func(exit string) bool {
		return exit == "intl" || exit == "cn" || exit == "codearts"
	}
	cases := []struct {
		name      string
		raw       string
		exit      string
		upstream  string
		canonical string
		qualified bool
		wantErr   bool
	}{
		{"registered exit", "cn/m1", "cn", "m1", "cn/m1", true, false},
		{"uppercase exit", "CN/m1", "cn", "m1", "cn/m1", true, false},
		{"whitespace and slash in upstream", " cn/foo/bar ", "cn", "foo/bar", "cn/foo/bar", true, false},
		{"bare model", "m1", "", "m1", "m1", false, false},
		{"bare model matching exit name", "cn", "", "cn", "cn", false, false},
		{"empty raw", "", "", "", "", false, false},
		{"registered exit with empty upstream", "cn/", "cn", "", "cn/", true, true},
		{"unknown prefix", "openai/gpt-4o", "", "openai/gpt-4o", "openai/gpt-4o", false, false},
		{"unknown prefix that is not a realm", "gateway/foo", "", "gateway/foo", "gateway/foo", false, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := parseModelRef(tt.raw, isExit)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseModelRef(%q) error = nil, want error", tt.raw)
				}
			} else if err != nil {
				t.Fatalf("parseModelRef(%q) error = %v", tt.raw, err)
			}
			if ref.Exit != tt.exit {
				t.Errorf("parseModelRef(%q).Exit = %q, want %q", tt.raw, ref.Exit, tt.exit)
			}
			if ref.Upstream != tt.upstream {
				t.Errorf("parseModelRef(%q).Upstream = %q, want %q", tt.raw, ref.Upstream, tt.upstream)
			}
			if ref.Canonical != tt.canonical {
				t.Errorf("parseModelRef(%q).Canonical = %q, want %q", tt.raw, ref.Canonical, tt.canonical)
			}
			if ref.Qualified != tt.qualified {
				t.Errorf("parseModelRef(%q).Qualified = %v, want %v", tt.raw, ref.Qualified, tt.qualified)
			}
		})
	}
}

func TestModelRefRequested(t *testing.T) {
	isExit := func(exit string) bool {
		return exit == "intl" || exit == "cn" || exit == "codearts"
	}
	qualified, err := parseModelRef("cn/m1", isExit)
	if err != nil {
		t.Fatalf("parseModelRef(%q) error = %v", "cn/m1", err)
	}
	if got := qualified.Requested(); got != "cn/m1" {
		t.Errorf("qualified.Requested() = %q, want %q", got, "cn/m1")
	}
	bare, err := parseModelRef("m1", isExit)
	if err != nil {
		t.Fatalf("parseModelRef(%q) error = %v", "m1", err)
	}
	if got := bare.Requested(); got != "m1" {
		t.Errorf("bare.Requested() = %q, want %q", got, "m1")
	}
}
