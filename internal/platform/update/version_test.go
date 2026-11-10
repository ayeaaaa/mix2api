package update

import "testing"

func TestParseVersionAndCompare(t *testing.T) {
	for _, text := range []string{"v1.2.3", "1.2.3"} {
		major, minor, patch, ok := ParseVersion(text)
		if !ok || major != 1 || minor != 2 || patch != 3 {
			t.Fatalf("%s = %d.%d.%d ok=%v", text, major, minor, patch, ok)
		}
	}
	for _, text := range []string{"v1.2", "1.2.3-rc1", "2026-01-02", "v1.2.3.4", "latest"} {
		if _, _, _, ok := ParseVersion(text); ok {
			t.Fatalf("%q accepted", text)
		}
	}
	if !IsNewer("v1.2.4", "1.2.3") || IsNewer("1.2.3", "v1.2.3") || IsNewer("bad", "1.0.0") {
		t.Fatal("IsNewer ordering wrong")
	}
	for _, test := range []struct {
		candidate, current string
		want               bool
	}{
		{"2.0.0", "2.0.0-rc.1", true},
		{"2.0.0-rc.1", "2.0.0", false},
		{"2.0.0-rc.2", "2.0.0-rc.1", true},
		{"2.0.0-rc.1", "2.0.0-rc.2", false},
		{"2.0.0-rc.1.1", "2.0.0-rc.1", true},
		{"2.0.0-rc.1", "2.0.0-rc.1", false},
		{"2.0.0", "2.0.0-dev", true},
		{"2.0.0-dev", "2.0.0-rc.1", false},
	} {
		if got := IsNewer(test.candidate, test.current); got != test.want {
			t.Fatalf("IsNewer(%q,%q)=%v want %v", test.candidate, test.current, got, test.want)
		}
	}
	for _, value := range []string{"2.0.0", "2.0.0-rc.1", "v2.0.0-rc.1"} {
		if !IsReleaseVersion(value) {
			t.Fatalf("IsReleaseVersion(%q)=false", value)
		}
	}
	for _, value := range []string{"2.0", "2.0.0+meta", "rc.1", ""} {
		if IsReleaseVersion(value) {
			t.Fatalf("IsReleaseVersion(%q)=true", value)
		}
	}
}

func TestPickLatestStable(t *testing.T) {
	releases := []any{
		map[string]any{"tag_name": "v1.9.9", "prerelease": true, "html_url": "https://github.com/x/y/releases/tag/v1.9.9"},
		map[string]any{"tag_name": "v2.0.0", "draft": true},
		map[string]any{"tag_name": "v1.2.0", "html_url": "https://github.com/x/y/releases/tag/v1.2.0", "published_at": "2026-01-01T00:00:00Z"},
		map[string]any{"tag_name": "v1.10.0", "html_url": "javascript:alert(1)"},
		map[string]any{"tag_name": "nightly"},
	}
	picked := PickLatest(releases, "1.9.0")
	if picked == nil || picked.Version != "1.10.0" || !picked.Newer || picked.URL != "" {
		t.Fatalf("picked = %#v", picked)
	}
	if picked := PickLatest(nil, "1.0.0"); picked != nil {
		t.Fatalf("empty picked = %#v", picked)
	}
	if url := SafeReleaseURL("http://github.com/x"); url != "" {
		t.Fatalf("unsafe url = %q", url)
	}
}

func TestFinalReleaseIsNewerThanRunningCandidate(t *testing.T) {
	picked := PickLatest([]any{
		map[string]any{"tag_name": "v2.0.0", "html_url": "https://github.com/ayeaaaa/mix2api-v2/releases/tag/v2.0.0"},
	}, "2.0.0-rc.1")
	if picked == nil || !picked.Newer || picked.Version != "2.0.0" {
		t.Fatalf("picked = %#v", picked)
	}
}
