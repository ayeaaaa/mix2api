package main

import (
	"io"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/version"
)

func TestVersionCommandTextJSONAndShort(t *testing.T) {
	oldVersion, oldCommit, oldDate := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = oldVersion, oldCommit, oldDate })
	version.Version, version.Commit, version.Date = "2.0.0-rc.1", "deadbeef", "2026-11-08T12:00:00Z"

	var out, errOut strings.Builder
	if code := runVersionCommand(nil, &out, &errOut); code != 0 {
		t.Fatalf("version exit = %d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "mix2api 2.0.0-rc.1") ||
		!strings.Contains(out.String(), "commit: deadbeef") ||
		!strings.Contains(out.String(), "built: 2026-11-08T12:00:00Z") {
		t.Fatalf("text output = %q", out.String())
	}

	out.Reset()
	if code := runVersionCommand([]string{"--json"}, &out, &errOut); code != 0 {
		t.Fatalf("json exit = %d", code)
	}
	if !strings.HasPrefix(out.String(), `{"version":"2.0.0-rc.1","commit":"deadbeef"`) {
		t.Fatalf("json output = %q", out.String())
	}

	out.Reset()
	if code := runVersionCommand([]string{"--short"}, &out, &errOut); code != 0 {
		t.Fatalf("short exit = %d", code)
	}
	if strings.TrimSpace(out.String()) != "2.0.0-rc.1" {
		t.Fatalf("short output = %q", out.String())
	}
}

func TestVersionCommandRejectsUnknownFlag(t *testing.T) {
	var out, errOut strings.Builder
	if code := runVersionCommand([]string{"--nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "flag provided but not defined") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestUnknownCommandDoesNotStartServer(t *testing.T) {
	var out, errOut strings.Builder
	if code := runCLI([]string{"definitely-not-a-command"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestVersionOutputIsMechanicallyInjected(t *testing.T) {
	// The variables, not constants, are the ldflags contract. This guards a
	// regression that would silently ignore -X at link time.
	old := version.Version
	version.Version = "9.9.9-test"
	defer func() { version.Version = old }()
	var out, errOut strings.Builder
	if code := runVersionCommand([]string{"--short"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.TrimSpace(out.String()) != "9.9.9-test" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestHelpListsCommands(t *testing.T) {
	var out, errOut strings.Builder
	if code := runCLI([]string{"help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"mix2api version", "mix2api update"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q: %s", want, out.String())
		}
	}
}

var _ io.Writer = (*strings.Builder)(nil)
