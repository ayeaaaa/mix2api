package migrate

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"accounts/settings.json":                                         `{"unknown_v1_key":{"nested":[1,2,3]},"panel_password_hash":"hash","api_keys":{"k1":{"realm":"intl"}}}`,
		"accounts/active_realm.json":                                     `{"realm":"codearts","updated_at":1793457803.9083126}`,
		"accounts/model.json":                                            `{"glm-5":[1000000,131072]}`,
		"accounts/model_windows.json":                                    `{"windows":[]}`,
		"accounts/quota_scan.json":                                       `{"rows":[]}`,
		"accounts/integration-state.json":                                `{"claude-code":{"applied_at":"2026-01-01 00:00:00","files":[]},"unknown":"keep"}`,
		"accounts/agent-backups/claude-code/20260101-000000-config.json": `{"original":true}`,
		"accounts/raccoon/raccoon-pu-test.json":                          `{"uid":"pu-test","access_token":"token","expires_at":4102444800,"unknown_account_key":true}`,
		"accounts/raccoon/state.json":                                    `{"pu-test":{"disabled":false,"manual_disabled":false}}`,
		"accounts/codearts/codearts-u1.json":                             `{"user_id":"u1","access_key_id":"ak","secret_access_key":"sk","unknown":1}`,
		"accounts/codearts/state.json":                                   `[{"name":"u1","disabled":false,"checkin":{"ok":true}}]`,
		"usage/usage.jsonl": "{\"account\":\"u1\",\"unknown_usage_key\":\"keep\",\"total_tokens\":5}\n" +
			"{\"account\":\"u2\",\"total_tokens\":7}\n",
		"usage/limit-events.jsonl":     "{\"kind\":\"limit\",\"at\":1}\n",
		"usage/pricing-timeline.jsonl": "{\"model\":\"glm-5\",\"input\":1}\n",
		"usage/usage-summary.json":     `{"legacy":"untouched"}`,
		"media/tasks.json":             `{"tasks":[{"id":"task-1","status":"completed"}]}`,
		"media/video-prices.json":      `{"video-model|720p":3}`,
		"updates/status.json":          `{"state":"idle","version":"","message":""}`,
		"updates/backups.json":         `{"backups":[]}`,
	}
	for name, content := range files {
		writeFixtureFile(t, filepath.Join(root, filepath.FromSlash(name)), content)
	}
	createSQLite(t, filepath.Join(root, "usage", "logs.sqlite3"))
	return root
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func createSQLite(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE logs (id INTEGER PRIMARY KEY, ts TEXT NOT NULL, time TEXT NOT NULL, level TEXT NOT NULL, tag TEXT NOT NULL, msg TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO logs (ts,time,level,tag,msg) VALUES ('1','2026-01-01','info','test','hello')`); err != nil {
		t.Fatal(err)
	}
}

func treeHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(relative)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrateCopiesEverythingAndPreservesUnknownKeys(t *testing.T) {
	root := fixtureRoot(t)
	before := treeHashes(t, root)
	dest := filepath.Join(t.TempDir(), "accounts-v2")
	summary, err := Migrate(Options{From: root, To: dest})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if summary.Direction != "migrate" || summary.Layout != "data" || summary.Files == 0 || summary.UsageRows != 2 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary.Accounts < 2 {
		t.Fatalf("account count = %d", summary.Accounts)
	}
	after := treeHashes(t, root)
	if len(before) != len(after) {
		t.Fatalf("source file count changed: %d -> %d", len(before), len(after))
	}
	for name, content := range before {
		if after[name] != content {
			t.Fatalf("source file %s changed", name)
		}
	}
	for name, content := range before {
		raw, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("destination missing %s: %v", name, err)
		}
		if string(raw) != content {
			t.Fatalf("destination %s differs", name)
		}
	}
	receiptPath := dest + ".migration.json"
	rawReceipt, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	var parsed receipt
	if err := json.Unmarshal(rawReceipt, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Format != ReceiptFormat || len(parsed.Files) != summary.Files {
		t.Fatalf("receipt = %#v", parsed)
	}
	if _, err := os.Stat(dest + lockSuffix); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	root := fixtureRoot(t)
	dest := filepath.Join(t.TempDir(), "out")
	summary, err := Migrate(Options{From: root, To: dest, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.DryRun || summary.Files == 0 {
		t.Fatalf("summary = %#v", summary)
	}
	for _, path := range []string{dest, dest + ".migration.json", dest + lockSuffix} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("dry run created %s: %v", path, err)
		}
	}
}

func TestInterruptedCopyLeavesNoDestination(t *testing.T) {
	root := fixtureRoot(t)
	dest := filepath.Join(t.TempDir(), "out")
	calls := 0
	_, err := Migrate(Options{From: root, To: dest, interrupt: func(path string, index int) error {
		calls++
		if calls == 3 {
			return os.ErrPermission
		}
		return nil
	}})
	if err == nil {
		t.Fatal("interrupted copy succeeded")
	}
	for _, path := range []string{dest, dest + ".migration.json", dest + lockSuffix} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("partial artifact %s survived: %v", path, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".partial-") {
			t.Fatalf("staging directory survived: %s", entry.Name())
		}
	}
}

func TestInvalidInputsAreRefused(t *testing.T) {
	for name, mutate := range map[string]func(string){
		"malformed settings": func(root string) {
			writeFixtureFile(t, filepath.Join(root, "accounts", "settings.json"), `{"broken":`)
		},
		"malformed usage row": func(root string) {
			writeFixtureFile(t, filepath.Join(root, "usage", "usage.jsonl"), "{\"ok\":1}\nnot-json\n")
		},
		"trailing JSONL garbage": func(root string) {
			writeFixtureFile(t, filepath.Join(root, "usage", "usage.jsonl"), "{\"ok\":1} trailing\n")
		},
		"corrupt sqlite": func(root string) {
			writeFixtureFile(t, filepath.Join(root, "usage", "logs.sqlite3"), "not a sqlite database")
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := fixtureRoot(t)
			mutate(root)
			dest := filepath.Join(t.TempDir(), "out")
			if _, err := Migrate(Options{From: root, To: dest}); err == nil {
				t.Fatal("invalid source accepted")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatalf("destination created for invalid source: %v", err)
			}
		})
	}
}

func TestDestinationMustBeNewAndOutsideSource(t *testing.T) {
	root := fixtureRoot(t)
	nonEmpty := t.TempDir()
	writeFixtureFile(t, filepath.Join(nonEmpty, "keep"), "x")
	if _, err := Migrate(Options{From: root, To: nonEmpty}); err == nil {
		t.Fatal("non-empty destination accepted")
	}
	if _, err := Migrate(Options{From: root, To: filepath.Join(root, "accounts", "copy")}); err == nil {
		t.Fatal("destination inside source accepted")
	}
	if _, err := Migrate(Options{From: root, To: root}); err == nil {
		t.Fatal("destination equal to source accepted")
	}
}

func TestLockConflictAndStaleLockCleanup(t *testing.T) {
	root := fixtureRoot(t)
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(dest+lockSuffix, []byte("pid=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(Options{From: root, To: dest}); err == nil {
		t.Fatal("lock conflict accepted")
	}
	if err := os.Remove(dest + lockSuffix); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(Options{From: root, To: dest}); err != nil {
		t.Fatalf("migration after lock removal: %v", err)
	}
}

func TestAccountsOnlyMigration(t *testing.T) {
	root := fixtureRoot(t)
	accounts := filepath.Join(root, "accounts")
	dest := filepath.Join(t.TempDir(), "accounts-v2")
	summary, err := Migrate(Options{From: accounts, To: dest})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Layout != "accounts" || summary.Accounts < 2 {
		t.Fatalf("summary = %#v", summary)
	}
	if _, err := os.Stat(filepath.Join(dest, "settings.json")); err != nil {
		t.Fatalf("accounts file not copied: %v", err)
	}
}

func TestRollbackCopiesCurrentV2DataBack(t *testing.T) {
	root := fixtureRoot(t)
	dest := filepath.Join(t.TempDir(), "v2")
	if _, err := Migrate(Options{From: root, To: dest}); err != nil {
		t.Fatal(err)
	}
	// A v2 process appended usage and created a v2-only log database.
	appendFile(t, filepath.Join(dest, "usage", "usage.jsonl"), "{\"account\":\"u3\",\"total_tokens\":9}\n")
	writeFixtureFile(t, filepath.Join(dest, "usage", "v2-cache.json"), `{"v2":true}`)

	restored := filepath.Join(t.TempDir(), "v1-restored")
	summary, err := Rollback(Options{From: dest, To: restored})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if summary.Direction != "rollback" {
		t.Fatalf("summary = %#v", summary)
	}
	rows, err := os.ReadFile(filepath.Join(restored, "usage", "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rows), "u3") {
		t.Fatalf("appended usage row lost: %s", rows)
	}
	if _, err := os.Stat(filepath.Join(restored, "usage", "v2-cache.json")); err != nil {
		t.Fatalf("v2 data file not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(restored, "accounts", "settings.json")); err != nil {
		t.Fatalf("settings not restored: %v", err)
	}
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLTrailingFragmentTolerated(t *testing.T) {
	root := fixtureRoot(t)
	when := filepath.Join(root, "usage", "usage.jsonl")
	appendFile(t, when, `{"partial":`)
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := Migrate(Options{From: root, To: dest}); err != nil {
		t.Fatalf("trailing fragment rejected: %v", err)
	}
}
