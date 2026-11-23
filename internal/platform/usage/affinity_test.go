package usage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRecorderAffinityDiagnosticsRoundtrip(t *testing.T) {
	store := New(Paths{Log: filepath.Join(t.TempDir(), "usage.jsonl")})
	r := Recorder{Store: store}
	if err := r.Record(context.Background(), RecordInput{Model: "deepseek", Realm: "intl", Kind: "chat", AffinityMode: "page", PrefixSHA: "abcdef0123456789", NMessages: 401, AffinityPoolSize: 3}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ReadRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["affinity_mode"] != "page" || rows[0]["prefix_sha"] != "abcdef0123456789" {
		t.Fatalf("diagnostics lost on read: %v", rows)
	}
	if rows[0]["affinity_fallback"] != false {
		t.Fatalf("false fallback lost: %v", rows[0])
	}
}
