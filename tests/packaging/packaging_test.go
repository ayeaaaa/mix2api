package packaging_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

func TestDockerfileIsMultiStageCGOFreeNonRootAndHealthchecked(t *testing.T) {
	dockerfile := repoFile(t, "Dockerfile")
	for _, want := range []string{
		"FROM golang:1.22-alpine AS build",
		"FROM alpine:3.20",
		"CGO_ENABLED=0",
		"TARGETOS",
		"TARGETARCH",
		"USER 10001:10001",
		"HEALTHCHECK",
		"http://127.0.0.1:${PORT}/health",
		`VOLUME ["/app/accounts", "/app/usage", "/app/media"]`,
		`ENTRYPOINT ["/app/mix2api"]`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile missing %q", want)
		}
	}
	if strings.Contains(dockerfile, "USER root") {
		t.Fatal("Dockerfile runs as root")
	}
	if strings.Contains(dockerfile, "chown -R mix2api:mix2api /app\n") || !strings.Contains(dockerfile, "chmod 0555 /app") {
		t.Fatal("application directory is not read-only")
	}
	dockerignore, err := os.ReadFile(filepath.Join("..", "..", ".dockerignore"))
	if err != nil || !strings.Contains(string(dockerignore), ".env") {
		t.Fatalf(".dockerignore must exclude .env: %v", err)
	}
}

func TestDockerfileDoesNotShadowBuildxTargetPlatform(t *testing.T) {
	dockerfile := repoFile(t, "Dockerfile")
	if strings.Contains(dockerfile, "ARG TARGETARCH=") || strings.Contains(dockerfile, "ARG TARGETOS=") {
		t.Fatal("Dockerfile must not give TARGETOS/TARGETARCH a default; that shadows buildx automatic platform args")
	}
	if !strings.Contains(dockerfile, "${TARGETARCH:-amd64}") || !strings.Contains(dockerfile, "${TARGETOS:-linux}") {
		t.Fatal("Dockerfile must fall back to amd64/linux only when buildx platform args are absent")
	}
}

func TestComposeAssetsMountDataAndHealthcheck(t *testing.T) {
	for _, name := range []string{"docker-compose.yml", "deploy/docker-compose.yml", "deploy/docker-compose.image.yml"} {
		text := repoFile(t, name)
		for _, want := range []string{
			"image: ghcr.io/ayeaaaa/mix2api-v2:",
			"/app/accounts",
			"/app/usage",
			"/app/media",
			"healthcheck:",
			"$${PORT}/health",
			"user:",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing %q", name, want)
			}
		}
	}
	env := repoFile(t, ".env.example")
	if !strings.Contains(env, "PANEL_PASSWORD=") || !strings.Contains(env, "MIX2API_API_KEY=") {
		t.Fatalf(".env.example missing credentials: %s", env)
	}
}

func TestOpenWrtAssets(t *testing.T) {
	initScript := repoFile(t, "packaging/openwrt/files/mix2api.init")
	for _, want := range []string{"USE_PROCD=1", "procd_open_instance", "procd_append_param env", "ACCOUNTS_DIR", "WB_MEDIA_DIR"} {
		if !strings.Contains(initScript, want) {
			t.Fatalf("init script missing %q", want)
		}
	}
	config := repoFile(t, "packaging/openwrt/files/mix2api.config")
	if !strings.Contains(config, "config mix2api 'main'") || !strings.Contains(config, "option port '58675'") {
		t.Fatalf("UCI config = %s", config)
	}
	makefile := repoFile(t, "packaging/openwrt/Makefile")
	for _, want := range []string{"include $(TOPDIR)/rules.mk", "MIX2API_BINARY", "mix2api.init", "mix2api.config", "BuildPackage", "PKGARCH:=$(ARCH)", "chmod 0600"} {
		if !strings.Contains(makefile, want) {
			t.Fatalf("OpenWrt Makefile missing %q", want)
		}
	}
}

func TestSystemdUnitIsHardened(t *testing.T) {
	unit := repoFile(t, "packaging/systemd/mix2api.service")
	for _, want := range []string{
		"User=mix2api", "Group=mix2api", "WorkingDirectory=/srv/mix2api",
		"EnvironmentFile=-/etc/mix2api/env", "ExecStart=/srv/mix2api/mix2api",
		"ACCOUNTS_DIR=/srv/mix2api/accounts", "USAGE_DIR=/srv/mix2api/usage",
		"WB_MEDIA_DIR=/srv/mix2api/media", "NoNewPrivileges=true",
		"ProtectSystem=strict", "ReadWritePaths=/srv/mix2api",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("systemd unit missing %q", want)
		}
	}
}

func TestSystemdUnitMatchesRunbookAndAnalyzes(t *testing.T) {
	unit := repoFile(t, "packaging/systemd/mix2api.service")
	runbook := repoFile(t, "docs/runbook.md")
	for _, want := range []string{
		"ExecStart=/srv/mix2api/mix2api",
		"ReadWritePaths=/srv/mix2api",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q", want)
		}
	}
	for _, want := range []string{
		"useradd --system", "/srv/mix2api/mix2api",
		"/srv/mix2api/accounts",
	} {
		if !strings.Contains(runbook, want) {
			t.Fatalf("runbook missing %q", want)
		}
	}
	if strings.Contains(runbook, "/var/lib/mix2api") {
		t.Fatal("runbook still documents the old /var/lib layout")
	}

	systemdAnalyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not installed")
	}
	// verify checks ExecStart exists; substitute a harmless existing binary
	// while retaining every directive and the /srv layout in the copy.
	verified := strings.ReplaceAll(unit, "ExecStart=/srv/mix2api/mix2api", "ExecStart=/bin/true")
	path := filepath.Join(t.TempDir(), "mix2api.service")
	if err := os.WriteFile(path, []byte(verified), 0o644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(systemdAnalyze, "verify", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("systemd-analyze verify: %v\n%s", err, output)
	}
}

func TestPortableLaunchersExistAndStayLocal(t *testing.T) {
	for _, name := range []string{
		"packaging/portable/start-mix2api.sh",
		"packaging/portable/stop-mix2api.sh",
		"packaging/portable/start-mix2api.cmd",
		"packaging/portable/stop-mix2api.cmd",
		"packaging/portable/README.md",
	} {
		info, err := os.Stat(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if strings.HasSuffix(name, ".sh") && info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable", name)
		}
	}
	start := repoFile(t, "packaging/portable/start-mix2api.sh")
	for _, want := range []string{"ACCOUNTS_DIR", "USAGE_DIR", "WB_MEDIA_DIR", "mix2api.pid", "exec \"$DIR/mix2api\""} {
		if !strings.Contains(start, want) {
			t.Fatalf("portable start missing %q", want)
		}
	}
	if strings.Contains(start, "python") {
		t.Fatal("portable launcher depends on Python")
	}
}

func TestMakefileTargets(t *testing.T) {
	makefile := repoFile(t, "Makefile")
	for _, want := range []string{
		"CGO_ENABLED=0 go build", "-trimpath", "-buildvcs=false",
		"CGO_ENABLED=1 go test -race ./...", "go vet ./...", "gofmt -l", "dist:",
	} {
		if !strings.Contains(makefile, want) {
			t.Fatalf("Makefile missing %q", want)
		}
	}
}

func TestReleaseBinariesCarryInjectedIdentityAndReproduce(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the full command twice")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	first := filepath.Join(dir, "mix2api-1")
	second := filepath.Join(dir, "mix2api-2")
	ldflags := "-s -w -buildid= " +
		"-X github.com/ayeaaaa/mix2api-v2/internal/core/version.Version=2.0.0-rc.1 " +
		"-X github.com/ayeaaaa/mix2api-v2/internal/core/version.Commit=abc123 " +
		"-X github.com/ayeaaaa/mix2api-v2/internal/core/version.Date=2026-11-08T00:00:00Z"
	build := func(output string) {
		t.Helper()
		command := exec.Command("go", "build", "-trimpath", "-buildvcs=false",
			"-buildmode=exe", "-ldflags", ldflags, "-o", output, "./cmd/mix2api")
		command.Dir = root
		command.Env = append(os.Environ(), "CGO_ENABLED=0")
		if raw, err := command.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, raw)
		}
	}
	build(first)
	build(second)
	firstRaw, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstRaw) != string(secondRaw) {
		t.Fatal("two fixed-metadata builds differ; reproducibility regressed")
	}
	output, err := exec.Command(first, "version", "--json").Output()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	var info struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Date    string `json:"date"`
	}
	if err := json.Unmarshal(output, &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != "2.0.0-rc.1" || info.Commit != "abc123" || info.Date != "2026-11-08T00:00:00Z" {
		t.Fatalf("injected identity = %#v", info)
	}
}
