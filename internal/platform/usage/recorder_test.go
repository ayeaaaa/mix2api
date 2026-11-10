package usage

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRecorderShape(t *testing.T) {
	env := newTestEnv(t, nil)
	recorder := &Recorder{Store: env.Store}
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	if err := recorder.Record(context.Background(), RecordInput{
		At: at, Model: "m", RequestedModel: "alias", Realm: "intl", Account: "a",
		KeyID: "k1", Kind: "chat", Outcome: "completed", Stream: true,
		Usage: map[string]any{
			"prompt_tokens": 100, "completion_tokens": 20, "cached_tokens": 40,
			"reasoning_tokens": 5, "total_tokens": 120, "credit": 1.5,
		},
		ElapsedMS: 2000, TTFTMS: 500, GenMS: 1000, Effort: "high",
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(env.Paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one row, got %d", len(lines))
	}
	row, ok := decodeUsageLine([]byte(lines[0]))
	if !ok {
		t.Fatal("row does not parse")
	}
	if row["model"] != "m" || row["requested_model"] != "alias" || row["stream"] != true {
		t.Fatalf("shape = %+v", row)
	}
	if row["cost_policy"] != "" || row["reasoning_effort"] != "high" {
		t.Fatalf("attribution = %+v", row)
	}
	if mustNumber(t, row["tokens_per_sec"]) != 20 || mustNumber(t, row["cache_hit_pct"]) != 40 {
		t.Fatalf("derived rates = %+v", row)
	}
	if _, exists := row["prompt"]; exists {
		t.Fatal("prompt text must never be stored")
	}
	if row["kind"] != "chat" || row["outcome"] != "completed" || row["key"] != "k1" {
		t.Fatalf("defaults = %+v", row)
	}

	if err := recorder.Record(context.Background(), RecordInput{
		At: at, Model: "m", Realm: "intl", KeyID: "",
	}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(env.Paths.Log)
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	missing, _ := decodeUsageLine([]byte(lines[len(lines)-1]))
	if missing["usage_missing"] != true || missing["key"] != "" {
		t.Fatalf("missing usage row = %+v", missing)
	}
	if _, exists := missing["requested_model"]; exists {
		t.Fatal("requested_model should only be written when different")
	}

	// Media and provider-managed realms get their explicit cost policies.
	media := &Recorder{Store: env.Store}
	if err := media.Record(context.Background(), RecordInput{
		At: at, Model: "image", Kind: "image", Realm: "intl", KeyID: "k",
		Media: map[string]any{"seconds": 5, "resolution": "720p", "aspect_ratio": "16:9", "image_to_video": true},
	}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(env.Paths.Log)
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	mediaRow, _ := decodeUsageLine([]byte(lines[len(lines)-1]))
	if mediaRow["cost_policy"] != "media" || mustNumber(t, mediaRow["seconds"]) != 5 {
		t.Fatalf("media row = %+v", mediaRow)
	}
	if err := media.Record(context.Background(), RecordInput{
		At: at, Model: "m", Realm: "codearts", KeyID: "k",
	}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(env.Paths.Log)
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	providerRow, _ := decodeUsageLine([]byte(lines[len(lines)-1]))
	if providerRow["cost_policy"] != "codearts:unpriced" {
		t.Fatalf("provider row = %+v", providerRow)
	}

	broken := &Recorder{Store: &Store{Paths: Paths{Log: ""}}}
	if err := broken.Record(context.Background(), RecordInput{Model: "m"}); err == nil {
		t.Fatal("append failure should be returned to the caller")
	}
}

func TestRecorderCachedTokenAliases(t *testing.T) {
	env := newTestEnv(t, nil)
	recorder := &Recorder{Store: env.Store}
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	if err := recorder.Record(context.Background(), RecordInput{
		At: at, Model: "m", Realm: "intl",
		Usage: map[string]any{
			"prompt_tokens": 100, "completion_tokens": 10, "reasoning_tokens": 0,
			"cached_tokens": 0, "cache_read_input_tokens": 0,
			"prompt_tokens_details":     map[string]any{"cached_tokens": 40},
			"completion_tokens_details": map[string]any{"reasoning_tokens": 7},
		},
		GenMS: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(env.Paths.Log)
	row, ok := decodeUsageLine([]byte(strings.TrimSpace(string(data))))
	if !ok {
		t.Fatal("row does not parse")
	}
	if mustNumber(t, row["cached_tokens"]) != 40 {
		t.Fatalf("cached alias not normalized: %+v", row)
	}
	if mustNumber(t, row["reasoning_tokens"]) != 7 {
		t.Fatalf("reasoning detail not normalized: %+v", row)
	}
	if mustNumber(t, row["cache_hit_pct"]) != 40 {
		t.Fatalf("cache hit pct = %v", row["cache_hit_pct"])
	}
}
