package logs

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sameReference(left, right any) bool {
	return reflect.ValueOf(left).Pointer() == reflect.ValueOf(right).Pointer()
}

func TestPhoneAndOAuthRedaction(t *testing.T) {
	phoneCases := []string{
		"13812345678",
		"138-1234-5678",
		"138 1234 5678",
		"138.1234.5678",
		"138·1234·5678",
		"138\t1234\t5678",
		"1381234 5678",
	}
	for _, value := range phoneCases {
		if got := RedactText(value); got != "138****5678" {
			t.Fatalf("RedactText(%q) = %q, want 138****5678", value, got)
		}
		if got := RedactText("账号 " + value + " 已导入"); got != "账号 138****5678 已导入" {
			t.Fatalf("embedded RedactText(%q) = %q", value, got)
		}
		if got := RedactText(RedactText(value)); got != "138****5678" {
			t.Fatalf("RedactText is not idempotent for %q: %q", value, got)
		}
	}

	unchanged := []string{
		"9" + strings.Repeat("0", 25),
		"ts=1700000000",
		"138123456789",
		"0138123456789",
		"113812345678",
		"6ef20bf4-76fa-4085-b179-97517476cc1e",
		"没有任何号码的一段话",
		"",
	}
	for _, value := range unchanged {
		if got := RedactText(value); got != value {
			t.Fatalf("RedactText(%q) changed to %q", value, got)
		}
	}
	if got := RedactText("账号179-97517476已停用"); got != "账号179****7476已停用" {
		t.Fatalf("prose separated phone = %q", got)
	}
	if got := RedactText("connect https://alice:hunter2@example.com/path"); got != "connect https://alice:<redacted>@example.com/path" {
		t.Fatalf("URL userinfo password not redacted: %q", got)
	}

	queryCases := map[string]string{
		"GET /oauth/callback?code=abc123&state=xyz":                  "GET /oauth/callback?code=<redacted>&state=<redacted>",
		"GET /auth/callback?authorization_code=abc&ticket_id=t1&x=1": "GET /auth/callback?authorization_code=<redacted>&ticket_id=<redacted>&x=1",
		"http://localhost/cb?SECRET=s3cr3t&Code=abc":                 "http://localhost/cb?SECRET=<redacted>&Code=<redacted>",
		"plain text without a query":                                 "plain text without a query",
	}
	for input, want := range queryCases {
		if got := RedactQuery(input); got != want {
			t.Fatalf("RedactQuery(%q) = %q, want %q", input, got, want)
		}
		if got := RedactQuery(RedactQuery(input)); got != want {
			t.Fatalf("RedactQuery is not idempotent for %q: %q", input, got)
		}
	}
}

func TestRedactPanelJSONCopyOnWrite(t *testing.T) {
	shared := map[string]any{"uid": "u1", "note": "plain"}
	value := map[string]any{
		"nickname": "13812345678",
		"accounts": []any{
			map[string]any{
				"uid":      "6ef20bf4-76fa-4085-b179-97517476cc1e",
				"account":  "13812345678",
				"id":       "138-1234-5678",
				"key":      "13812345678",
				"token":    "13812345678",
				"path":     "13812345678",
				"nickname": "老王(13812345678)",
			},
		},
		"logs":  []any{"account 13812345678 imported"},
		"msg":   "call 138-1234-5678 now",
		"plain": "no phone here",
		"child": shared,
	}
	snapshot, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	redacted := RedactPanelJSON(value)
	output, ok := redacted.(map[string]any)
	if !ok {
		t.Fatalf("RedactPanelJSON returned %T", redacted)
	}
	if output["nickname"] != "138****5678" {
		t.Fatalf("nickname = %#v", output["nickname"])
	}
	accounts := output["accounts"].([]any)
	account := accounts[0].(map[string]any)
	if account["nickname"] != "老王(138****5678)" {
		t.Fatalf("nested nickname = %#v", account["nickname"])
	}
	for _, key := range []string{"uid", "account", "id", "key", "token", "path"} {
		original := value["accounts"].([]any)[0].(map[string]any)[key]
		if account[key] != original {
			t.Fatalf("identity key %s changed from %#v to %#v", key, original, account[key])
		}
	}
	if !strings.Contains(output["logs"].([]any)[0].(string), "138****5678") {
		t.Fatalf("nested log not redacted: %#v", output["logs"])
	}
	if !strings.Contains(output["msg"].(string), "138****5678") {
		t.Fatalf("msg not redacted: %#v", output["msg"])
	}
	if output["plain"] != "no phone here" {
		t.Fatalf("phone-free value changed: %#v", output["plain"])
	}
	if !sameReference(output["child"], shared) {
		t.Fatal("untouched branch lost its identity")
	}

	after, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(snapshot) {
		t.Fatalf("input object was mutated:\nbefore %s\nafter  %s", snapshot, after)
	}
	if sameReference(redacted, value) {
		t.Fatal("changed object was returned by reference")
	}

	typed := map[string]string{"nickname": "13812345678", "uid": "u1", "msg": "call 138-1234-5678"}
	typedRedacted := RedactPanelJSON(typed).(map[string]string)
	if typedRedacted["nickname"] != "138****5678" {
		t.Fatalf("typed nickname = %q", typedRedacted["nickname"])
	}
	if typedRedacted["uid"] != "u1" || !strings.Contains(typedRedacted["msg"], "138****5678") {
		t.Fatalf("typed map = %#v", typedRedacted)
	}
	if typed["nickname"] != "13812345678" {
		t.Fatal("typed map input was mutated")
	}

	noHit := map[string]any{"uid": "u1", "count": 2, "note": "hello"}
	if got := RedactPanelJSON(noHit); !sameReference(got, noHit) {
		t.Fatal("no-hit object was copied")
	}
	list := []any{"hello", int64(2)}
	if got := RedactPanelJSON(list); !sameReference(got, list) {
		t.Fatal("no-hit slice was copied")
	}

	once, _ := json.Marshal(redacted)
	twice, _ := json.Marshal(RedactPanelJSON(redacted))
	if string(once) != string(twice) {
		t.Fatalf("panel redaction is not idempotent:\n%s\n%s", once, twice)
	}
}

func TestAppendInfersBeforeRedaction(t *testing.T) {
	store := Open(filepath.Join(t.TempDir(), "logs.sqlite3"), false, Caps{}, nil)
	defer store.Close()

	// "11101" is an ERROR hint. Masking it before inference would downgrade
	// the row to INFO, so the order of operations is observable.
	entry := Entry{Msg: "login 13811101123 failed"}
	if err := store.Append(entry); err != nil {
		t.Fatalf("Append: %v", err)
	}
	result, err := store.Query(Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if result.Total != 1 {
		t.Fatalf("total = %d", result.Total)
	}
	got := result.Logs[0]
	if got.Level != "ERROR" {
		t.Fatalf("level = %q, want ERROR", got.Level)
	}
	if got.Msg != "login 138****1123 failed" {
		t.Fatalf("message = %q, want masked", got.Msg)
	}

	if err := store.Append(Entry{Msg: "callback /oauth/callback?code=abc&state=xyz"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	result, err = store.Query(Query{Search: "callback"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if strings.Contains(result.Logs[0].Msg, "code=abc") || !strings.Contains(result.Logs[0].Msg, "code=<redacted>") {
		t.Fatalf("OAuth query not redacted: %q", result.Logs[0].Msg)
	}
}
