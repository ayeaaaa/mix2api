package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func systemdManager(t *testing.T) (*RequestManager, string) {
	t.Helper()
	dir := t.TempDir()
	unit := filepath.Join(t.TempDir(), "updater.path")
	link := filepath.Join(t.TempDir(), "updater.path")
	if err := os.WriteFile(unit, []byte("unit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("link"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB_UPDATE_INSTALL_ENABLED", "1")
	executor := &Executor{Dir: dir, UnitPath: unit, EnabledLink: link, AllowOwner: func(uint32) bool { return true }}
	now := time.Unix(1700000000, 0)
	manager := NewRequestManager(RequestManagerOptions{
		Dir: dir, Executor: executor, Now: func() time.Time { return now },
		PID: 1234, Port: 8788, Root: "/srv/mix2api",
	})
	return manager, dir
}

func TestInstallValidationAndRequestFile(t *testing.T) {
	manager, dir := systemdManager(t)
	if !manager.Enabled() || manager.Kind() != "systemd" {
		t.Fatalf("kind = %q enabled=%v", manager.Kind(), manager.Enabled())
	}
	if _, err := manager.Install("v1.0.0", "1.0.0", 8788, false, "x/y"); err != ErrTargetInvalid {
		t.Fatalf("not-newer err = %v", err)
	}
	if _, err := manager.Install("v1.2.0-rc1", "1.0.0", 8788, false, "x/y"); err != ErrTargetInvalid {
		t.Fatalf("prerelease err = %v", err)
	}
	installation, err := manager.Install("v1.2.3", "1.0.0", 8788, false, "x/y")
	if err != nil || installation.State != "queued" || installation.Version != "1.2.3" {
		t.Fatalf("install = %#v err=%v", installation, err)
	}
	path := filepath.Join(dir, RequestFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("request mode = %o", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	var request Request
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.Action != "install" || request.Tag != "v1.2.3" || request.Version != "1.2.3" ||
		request.PID != 1234 || request.Port != 8788 || request.Root != "/srv/mix2api" || request.RequestedAt == "" {
		t.Fatalf("request = %#v", request)
	}
	if _, err := manager.Install("v1.2.4", "1.0.0", 8788, false, "x/y"); err != ErrRequestActive {
		t.Fatalf("active request err = %v", err)
	}
	if manager.Status().State != "queued" {
		t.Fatalf("status = %#v", manager.Status())
	}
	// No overwrite: the first request bytes survive.
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("request was overwritten")
	}
}

func writeStatus(t *testing.T, dir string, value map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(value)
	if err := os.WriteFile(filepath.Join(dir, StatusFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackValidation(t *testing.T) {
	manager, dir := systemdManager(t)
	writeStatus(t, dir, map[string]any{"state": "idle"})
	if _, err := manager.Rollback("1.0.0", "1.1.0", 8788, false); err == nil || !strings.Contains(err.Error(), "没有可回退") {
		t.Fatalf("missing backup err = %v", err)
	}
	backups := map[string]any{"backups": []any{
		map[string]any{"version": "1.0.0", "complete": true, "files": 10, "created_at": "2026-01-01"},
		map[string]any{"version": "1.0.1", "complete": false, "files": 2},
	}}
	raw, _ := json.Marshal(backups)
	if err := os.WriteFile(filepath.Join(dir, BackupsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rollback("1.0.1", "1.1.0", 8788, false); err == nil {
		t.Fatal("incomplete backup accepted")
	}
	if _, err := manager.Rollback("9.9.9", "1.1.0", 8788, false); err == nil {
		t.Fatal("arbitrary version accepted")
	}
	if _, err := manager.Rollback("1.1.0", "1.1.0", 8788, false); err == nil {
		t.Fatal("same version accepted")
	}
	installation, err := manager.Rollback("1.0.0", "1.1.0", 8788, false)
	if err != nil || installation.State != "queued" {
		t.Fatalf("rollback = %#v err=%v", installation, err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, RequestFile))
	var request Request
	_ = json.Unmarshal(raw, &request)
	if request.Action != "rollback" || request.Tag != "v1.0.0" || request.Version != "1.0.0" {
		t.Fatalf("rollback request = %#v", request)
	}
}

func TestStatusAndBackupReaders(t *testing.T) {
	manager, dir := systemdManager(t)
	if manager.Status().State != "idle" {
		t.Fatalf("missing status = %#v", manager.Status())
	}
	writeStatus(t, dir, map[string]any{"state": "queued", "version": "1.2.3", "message": "wait"})
	if status := manager.Status(); status.State != "queued" || status.Version != "1.2.3" {
		t.Fatalf("status = %#v", status)
	}
	if err := os.Chmod(filepath.Join(dir, StatusFile), 0o666); err != nil {
		t.Fatal(err)
	}
	if status := manager.Status(); status.State != "idle" {
		t.Fatalf("world-writable status accepted: %#v", status)
	}
	if err := os.Chmod(filepath.Join(dir, StatusFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, StatusFile), []byte(`{"state":"bogus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := manager.Status(); status.State != "idle" {
		t.Fatalf("unknown state accepted: %#v", status)
	}

	backups := map[string]any{"backups": []any{
		map[string]any{"version": "1.2.3", "complete": true, "files": 3, "size_mb": 20, "created_at": "2026-01-01", "path": "/secret/path"},
		map[string]any{"version": "bad", "complete": true},
		map[string]any{"version": "1.2.2", "complete": false},
	}}
	raw, _ := json.Marshal(backups)
	if err := os.WriteFile(filepath.Join(dir, BackupsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	inventory := manager.Backups()
	if inventory.Source != "systemd" || len(inventory.Backups) != 2 {
		t.Fatalf("inventory = %#v", inventory)
	}
	encoded, _ := json.Marshal(inventory)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "path") {
		t.Fatalf("backup path leaked: %s", encoded)
	}
	if err := os.Chmod(filepath.Join(dir, BackupsFile), 0o664); err != nil {
		t.Fatal(err)
	}
	if len(manager.Backups().Backups) != 0 {
		t.Fatal("group-writable backups accepted")
	}
}

func TestRequestFileSymlinkRefusedAndMissingDir(t *testing.T) {
	manager, dir := systemdManager(t)
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, RequestFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install("v2.0.0", "1.0.0", 8788, false, ""); err != ErrRequestActive {
		t.Fatalf("symlink err = %v", err)
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "do-not-touch" {
		t.Fatal("symlink target was written")
	}

	missing := NewRequestManager(RequestManagerOptions{Dir: filepath.Join(t.TempDir(), "missing"), Executor: &Executor{Dir: filepath.Join(t.TempDir(), "missing")}})
	if _, err := missing.Install("v2.0.0", "1.0.0", 8788, false, ""); err != ErrInstallDisabled {
		t.Fatalf("missing dir err = %v", err)
	}
}

func TestExecutorKinds(t *testing.T) {
	t.Setenv("WB_UPDATE_INSTALL_ENABLED", "")
	dir := t.TempDir()
	if kind := NewExecutor(dir).Kind(); kind != "none" {
		t.Fatalf("empty kind = %q", kind)
	}
	if err := os.WriteFile(filepath.Join(dir, PortableMarker), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if kind := NewExecutor(dir).Kind(); kind != "portable" {
		t.Fatalf("portable kind = %q", kind)
	}
	if err := os.Remove(filepath.Join(dir, PortableMarker)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ExecutorMarker), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB_UPDATE_INSTALL_ENABLED", "1")
	if kind := NewExecutor(dir).Kind(); kind != "external" {
		t.Fatalf("external kind = %q", kind)
	}
}

func TestExecutorContractDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/update-executor-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"request.json", "status.json", "backups.json", "O_EXCL|O_NOFOLLOW",
		"action", "rollback", "complete", "WB_UPDATE_EXECUTOR=external",
		"WB_UPDATE_PORTABLE=1", "WB_UPDATE_ALLOW_CURRENT_OWNER=1",
		"succeeded", "rolled_back", "failed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("contract missing %q", want)
		}
	}
}

func TestBackupsLocalSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WB_UPDATE_INSTALL_ENABLED", "1")
	t.Setenv("WB_UPDATE_ALLOW_CURRENT_OWNER", "1")
	if err := os.WriteFile(filepath.Join(dir, ExecutorMarker), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewRequestManager(RequestManagerOptions{Dir: dir})
	raw, _ := json.Marshal(map[string]any{"backups": []any{map[string]any{"version": "1.0.0", "complete": true, "files": 1}}})
	if err := os.WriteFile(filepath.Join(dir, BackupsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	inventory := manager.Backups()
	if inventory.Source != "local" || len(inventory.Backups) != 1 || inventory.Backups[0].Source != "local" {
		t.Fatalf("local inventory = %#v", inventory)
	}
	t.Setenv("WB_UPDATE_INSTALL_ENABLED", "")
	if inventory := manager.Backups(); inventory.Source != "" || len(inventory.Backups) != 0 {
		t.Fatalf("empty inventory = %#v", inventory)
	}
}

func TestStatusUpdatedAt(t *testing.T) {
	_, dir := systemdManager(t)
	writeStatus(t, dir, map[string]any{
		"state": "succeeded", "version": "1.2.3", "message": "ok",
		"updated_at": "2026-01-02 03:04:05",
	})
	status, ok := (&Executor{Dir: dir, UnitPath: "/none", AllowOwner: func(uint32) bool { return true }}).ReadStatus()
	if !ok || status.UpdatedAt != "2026-01-02 03:04:05" {
		t.Fatalf("status = %#v ok=%v", status, ok)
	}
}

func TestReleaseCandidateToFinalUpgradeAndRollback(t *testing.T) {
	manager, dir := systemdManager(t)
	installation, err := manager.Install("v2.0.0", "2.0.0-rc.1", 8788, false, "x/y")
	if err != nil {
		t.Fatalf("RC->final install: %v", err)
	}
	if installation.State != "queued" || installation.Version != "2.0.0" {
		t.Fatalf("installation = %#v", installation)
	}
	_ = os.Remove(filepath.Join(dir, RequestFile))
	if _, err := manager.Install("v2.0.0", "2.0.0", 8788, false, "x/y"); err != ErrTargetInvalid {
		t.Fatalf("same-version install err = %v", err)
	}
	_ = os.Remove(filepath.Join(dir, RequestFile))

	raw, _ := json.Marshal(map[string]any{"backups": []any{map[string]any{
		"version": "2.0.0-rc.1", "created_at": "2026-11-09 00:00:00",
		"files": 4, "complete": true, "size_mb": 20,
	}}})
	if err := os.WriteFile(filepath.Join(dir, BackupsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	installation, err = manager.Rollback("2.0.0-rc.1", "2.0.0", 8788, false)
	if err != nil {
		t.Fatalf("final->RC rollback: %v", err)
	}
	if installation.Version != "2.0.0-rc.1" {
		t.Fatalf("rollback = %#v", installation)
	}
}
