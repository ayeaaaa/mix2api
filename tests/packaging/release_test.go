package packaging_test

import (
	"os"
	"os/exec"
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
		`RELEASE_REPOSITORY: "ayeaaaa/mix2api"`,
		`IMAGE_REPOSITORY: "ghcr.io/ayeaaaa/mix2api"`,
		`tags: ${{ env.IMAGE_REPOSITORY }}:v${{ inputs.version }}`,
		"Validate version input", "VERSION: ${{ inputs.version }}",
		`./scripts/validate-release-version.sh "$VERSION"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("release workflow missing %q", want)
		}
	}
	if strings.Contains(text, "\n  push:\n") || strings.Contains(text, "\n  tags:\n") || strings.Contains(text, "\n  schedule:\n") {
		t.Fatal("release workflow has an automatic trigger")
	}
	if strings.Count(text, "if: ${{ inputs.publish && inputs.gate_f_signed == 'yes' }}") < 2 {
		t.Fatal("container and publish jobs must both require explicit publish + Gate F")
	}
	if strings.Contains(text, "github.repository") && strings.Contains(text, "ghcr.io/${{ github.repository }}") {
		t.Fatal("container image still uses the runner repository identity")
	}
}

func TestReleaseDocumentsAndCurrentVersion(t *testing.T) {
	for _, name := range []string{"CHANGELOG.md", "SECURITY.md", "docs/release.md", "docs/runbook.md", "docs/gate-f.md"} {
		if _, err := os.Stat(filepath.Join("..", "..", name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	changelog := repoFile(t, "CHANGELOG.md")
	if !strings.Contains(changelog, "## [2.0.0]") {
		t.Fatalf("CHANGELOG does not describe the final release: %s", changelog)
	}
	gate := repoFile(t, "docs/gate-f.md")
	if !strings.Contains(gate, "PENDING") {
		t.Fatal("Gate F document must keep unverified gates marked PENDING")
	}
	release := repoFile(t, "docs/release.md")
	if !strings.Contains(release, "v2.0.0") {
		t.Fatal("release doc must describe the v2.0.0 release")
	}
}

func TestReleaseIdentityIsUnified(t *testing.T) {
	releaseSource := repoFile(t, "internal/core/release/release.go")
	if !strings.Contains(releaseSource, `Repository = "ayeaaaa/mix2api"`) {
		t.Fatal("core/release repository identity mismatch")
	}
	updaterSource := repoFile(t, "internal/platform/update/checker.go")
	if !strings.Contains(updaterSource, `DefaultRepository   = "ayeaaaa/mix2api"`) {
		t.Fatal("update checker repository identity mismatch")
	}
	compose := repoFile(t, "docker-compose.yml")
	if !strings.Contains(compose, "ghcr.io/ayeaaaa/mix2api:") {
		t.Fatal("compose image identity mismatch")
	}
	workflow := repoFile(t, ".github/workflows/release.yml")
	if !strings.Contains(workflow, "ayeaaaa/mix2api") {
		t.Fatal("workflow repository identity mismatch")
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

func TestValidateReleaseVersionScript(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "validate-release-version.sh"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("release version script is not executable")
	}
	for _, good := range []string{"2.0.0", "2.0.0-rc.1", "10.20.30", "0.1.0-alpha.1"} {
		if output, err := exec.Command(script, good).CombinedOutput(); err != nil {
			t.Fatalf("version %q rejected: %v\n%s", good, err, output)
		}
	}
	for _, bad := range []string{"", "v2.0.0", "V2.0.0", "2.0", "2.0.0+build", "01.2.3", "latest", "2.0.0\nrm -rf /", "2.0.0 ", "2.0.0;id"} {
		if output, err := exec.Command(script, bad).CombinedOutput(); err == nil {
			t.Fatalf("version %q accepted: %s", bad, output)
		}
	}
}

func TestReleaseWorkflowYAMLParses(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	workflow, err := filepath.Abs(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	script := `import sys, yaml
with open(sys.argv[1], encoding="utf-8") as fh:
    doc = yaml.safe_load(fh)
assert isinstance(doc, dict), doc
jobs = doc.get("jobs") or {}
assert {"verify", "container", "publish"} <= set(jobs), jobs.keys()
`
	command := exec.Command(python, "-c", script, workflow)
	if output, err := command.CombinedOutput(); err != nil {
		if strings.Contains(string(output), "No module named 'yaml'") {
			t.Skip("PyYAML not installed")
		}
		t.Fatalf("release workflow YAML invalid: %v\n%s", err, output)
	}
}
