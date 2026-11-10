package packaging_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseWorkflowIsManualAndGateFGuarded(t *testing.T) {
	text := repoFile(t, ".github/workflows/release.yml")
	for _, want := range []string{
		"workflow_dispatch:", "gate_f_signed", "publish:", "prerelease:",
		"make check", "make race", "make dist", "SHA256SUMS", "sbom.spdx.json",
		"docker/build-push-action", "gh release create", "Gate F signoff",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("release workflow missing %q", want)
		}
	}
	if strings.Contains(text, "\n  push:\n") || strings.Contains(text, "\n  tags:\n") || strings.Contains(text, "\n  schedule:\n") {
		t.Fatal("release workflow has an automatic trigger")
	}
	if !strings.Contains(text, "if: ${{ inputs.publish && inputs.gate_f_signed == 'yes' }}") {
		t.Fatal("publish job is not gated on explicit publish + Gate F")
	}
}

func TestReleaseDocumentsAndCurrentVersion(t *testing.T) {
	for _, name := range []string{"CHANGELOG.md", "SECURITY.md", "docs/release.md", "docs/runbook.md", "docs/gate-f.md"} {
		if _, err := os.Stat(filepath.Join("..", "..", name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	changelog := repoFile(t, "CHANGELOG.md")
	if !strings.Contains(changelog, "2.0.0-rc.1") {
		t.Fatalf("CHANGELOG does not describe the RC: %s", changelog)
	}
	gate := repoFile(t, "docs/gate-f.md")
	if !strings.Contains(gate, "PENDING") {
		t.Fatal("Gate F document must keep unverified gates marked PENDING")
	}
	release := repoFile(t, "docs/release.md")
	if !strings.Contains(release, "No `v2.0.0` tag") {
		t.Fatal("release doc must not claim v2.0.0 is released")
	}
}

func TestReadmeDescribesPackagingAndVersion(t *testing.T) {
	readme := repoFile(t, "README.md")
	for _, want := range []string{"mix2api version", "make dist", "mix2api migrate", "Dockerfile"} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README missing %q", want)
		}
	}
}
