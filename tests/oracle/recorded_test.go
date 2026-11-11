package oracle_test

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/tests/contract"
	harness "github.com/ayeaaaa/mix2api-v2/tests/oracle/harness"
)

const recordedDir = "recorded/v1.6.5"

var expectedCases = []string{
	"models",
	"codearts_models",
	"health",
	"chat",
	"chat_stream",
	"responses",
	"responses_stream",
	"messages",
	"codearts_chat",
	"codearts_chat_stream",
	"admin_accounts",
	"codearts_accounts",
	"media_models",
	"agents",
	"updates",
	"updates_backups",
	"codearts_overview",
	"codearts_checkin",
	"codearts_credits_refresh",
	"usage",
	"usage_raccoon",
	"usage_codearts",
	"logs",
	"codearts_models_refresh",
}

// TestRecordedV165OracleIntegrity verifies the committed v1.6.5 fixture without
// contacting any target: the manifest is well formed, every checksum matches,
// provenance is fake-upstream/loopback-only, and no credential or external URL
// was captured.
func TestRecordedV165OracleIntegrity(t *testing.T) {
	document, err := harness.Load(recordedDir)
	if err != nil {
		t.Fatalf("load recorded oracle: %v", err)
	}
	provenance := document.Provenance
	if provenance.TargetVersion != "1.6.5" {
		t.Fatalf("target_version = %q", provenance.TargetVersion)
	}
	if provenance.TargetCommit != "e40c4e8994f9cc6ace3457e350466cdd5d422a37" {
		t.Fatalf("target_commit = %q", provenance.TargetCommit)
	}
	if !provenance.FakeUpstream {
		t.Fatal("provenance does not declare a fake upstream")
	}
	if provenance.Network != "loopback-only" {
		t.Fatalf("network = %q", provenance.Network)
	}
	if provenance.BaseURL != "http://127.0.0.1:18765" {
		t.Fatalf("base_url = %q", provenance.BaseURL)
	}
	if !strings.Contains(strings.ToLower(provenance.Note), "loopback") {
		t.Fatalf("note does not mention loopback: %q", provenance.Note)
	}

	names := make([]string, 0, len(document.Cases))
	for _, item := range document.Cases {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	expected := append([]string(nil), expectedCases...)
	sort.Strings(expected)
	if strings.Join(names, ",") != strings.Join(expected, ",") {
		t.Fatalf("case set = %v\nwant %v", names, expected)
	}
	if len(document.Cases) != len(expectedCases) {
		t.Fatalf("case count = %d, want %d", len(document.Cases), len(expectedCases))
	}

	seen := map[string]bool{}
	for index, item := range document.Cases {
		if seen[item.Name] {
			t.Fatalf("duplicate case %q", item.Name)
		}
		seen[item.Name] = true
		if item.Kind != "json" && item.Kind != "sse" {
			t.Fatalf("%s kind = %q", item.Name, item.Kind)
		}
		if strings.Contains(item.Path, "://") || !strings.HasPrefix(item.Path, "/") {
			t.Fatalf("%s path is not a local route: %q", item.Name, item.Path)
		}
		if item.Status < 200 || item.Status > 599 {
			t.Fatalf("%s status = %d", item.Name, item.Status)
		}
		if item.Name == "media_models" && item.Status != 404 {
			t.Fatalf("media_models status = %d, want the recorded disabled-media 404", item.Status)
		}
		if item.Name == "health" {
			if index == 0 || document.Cases[index-1].Name != "codearts_models" {
				t.Fatalf("health must be recorded after both models cases; got %q before it",
					document.Cases[index-1].Name)
			}
		}
	}

	rawFiles, err := os.ReadDir(recordedDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range rawFiles {
		if entry.IsDir() || entry.Name() == "oracle.json" || strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(recordedDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			t.Fatalf("%s is empty", entry.Name())
		}
		if strings.HasSuffix(entry.Name(), ".sse") && !strings.Contains(string(body), "data:") {
			t.Fatalf("%s does not look like an SSE stream", entry.Name())
		}
	}
}

// TestRecordedV165DocumentsV2Delta keeps the reviewed comparison output and
// its provenance note in sync with the committed case set.
func TestRecordedV165DocumentsV2Delta(t *testing.T) {
	delta, err := os.ReadFile(filepath.Join(recordedDir, "V2-DELTA.txt"))
	if err != nil {
		t.Fatalf("read V2-DELTA.txt: %v", err)
	}
	text := string(delta)
	for _, needle := range []string{
		"2.0.0-rc.1",
		"10 case(s) differ",
		"Resolved by this candidate:",
		"oracle: health: JSON mismatch:",
		"oracle: logs: JSON mismatch:",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("V2-DELTA.txt does not contain %q", needle)
		}
	}
	if strings.Contains(text, "oracle: agents: JSON mismatch:") {
		t.Fatal("V2-DELTA.txt still reports the resolved agents mismatch")
	}
	provenance, err := os.ReadFile(filepath.Join(recordedDir, "PROVENANCE.md"))
	if err != nil {
		t.Fatalf("read PROVENANCE.md: %v", err)
	}
	if !strings.Contains(string(provenance), "V2-DELTA.txt") {
		t.Fatal("PROVENANCE.md does not reference V2-DELTA.txt")
	}
}

// TestRecordedV165IsDesensitized scans every committed byte for credentials and
// non-loopback URLs. The oracle run used only the fake upstream and synthetic
// accounts, so any hit is a fixture bug.
func TestRecordedV165IsDesensitized(t *testing.T) {
	banned := []string{
		"oracle-synthetic-api-key",
		"oracle-synthetic-panel-password",
		"Bearer ",
		"refresh-1",
		`"access_token"`,
		`"cloud_dragon_token"`,
		`"secret_access_key"`,
		`"dpop_private_key"`,
		"eyJ",
	}
	urlPattern := regexp.MustCompile(`https?://[^\s"'<>]+`)
	entries, err := os.ReadDir(recordedDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(recordedDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, needle := range banned {
			if strings.Contains(text, needle) {
				t.Fatalf("%s contains banned credential pattern %q", entry.Name(), needle)
			}
		}
		for _, rawURL := range urlPattern.FindAllString(text, -1) {
			rawURL = strings.TrimRight(rawURL, ".,);]")
			parsed, err := url.Parse(rawURL)
			if err != nil {
				t.Fatalf("%s has an unparseable URL %q: %v", entry.Name(), rawURL, err)
			}
			host := strings.ToLower(parsed.Hostname())
			if host != "127.0.0.1" && host != "localhost" && host != "::1" {
				t.Fatalf("%s contains non-loopback URL %q", entry.Name(), rawURL)
			}
		}
	}
}

// TestRecordedV165PayloadsNormalize proves every captured body is valid JSON or
// SSE under the same normalizer the comparator uses.
func TestRecordedV165PayloadsNormalize(t *testing.T) {
	document, err := harness.Load(recordedDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range document.Cases {
		body, err := os.ReadFile(filepath.Join(recordedDir, item.File))
		if err != nil {
			t.Fatal(err)
		}
		switch item.Kind {
		case "sse":
			events, err := contract.NormalizeSSE(body)
			if err != nil {
				t.Fatalf("%s: %v", item.Name, err)
			}
			if len(events) == 0 {
				t.Fatalf("%s: no SSE events", item.Name)
			}
		case "json":
			if _, err := contract.NormalizeJSON(body); err != nil {
				t.Fatalf("%s: %v", item.Name, err)
			}
		default:
			t.Fatalf("%s: unknown kind %q", item.Name, item.Kind)
		}
	}
}
