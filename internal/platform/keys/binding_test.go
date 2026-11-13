package keys

import (
	"strings"
	"sync"
	"testing"
)

func TestKeyModelAllowlist(t *testing.T) {
	unrestricted := Entry{}
	if !AllowsModel(unrestricted, "") || !AllowsModel(unrestricted, "any-model") {
		t.Fatal("empty allow-list should be unrestricted")
	}
	entry := Entry{Models: NormalizeModelPatterns("DeepSeek/*; gpt-6-astra\nDEEPSEEK/*")}
	if len(entry.Models) != 2 {
		t.Fatalf("patterns = %#v", entry.Models)
	}
	for _, model := range []string{"deepseek/chat", "DeepSeek/Reasoner", "gpt-6-astra", "GPT-6-ASTRA"} {
		if !AllowsModel(entry, model) {
			t.Fatalf("model %q rejected", model)
		}
	}
	if AllowsModel(entry, "gpt-6-pro") {
		t.Fatal("unlisted model accepted")
	}
	if AllowsModel(entry, "  ") {
		t.Fatal("empty model accepted by restricted key")
	}
	class := Entry{Models: []string{"model-[ab]?"}}
	if !AllowsModel(class, "model-a1") || AllowsModel(class, "model-c1") {
		t.Fatal("character class matching is wrong")
	}
}

func TestAllowsModelAny(t *testing.T) {
	bare := Entry{Models: NormalizeModelPatterns("deepseek/*")}
	// A legacy bare pattern keeps matching the upstream fallback. The fallback
	// here is a provider-qualified upstream id, the shape such a pattern can
	// match under fnmatch (a pattern's slash is literal).
	if !AllowsModelAny(bare, "cn/deepseek-v4.1-flash", "deepseek/deepseek-v4.1-flash") {
		t.Fatal("upstream fallback should allow canonical request")
	}
	// The same guarantee for the real WorkBuddy pairing: a legacy pattern that
	// matches the bare upstream id alone still admits the qualified request.
	legacy := Entry{Models: NormalizeModelPatterns("deepseek-*")}
	if !AllowsModelAny(legacy, "cn/deepseek-v4.1-flash", "deepseek-v4.1-flash") {
		t.Fatal("legacy upstream-only pattern rejected qualified request")
	}
	qualified := Entry{Models: NormalizeModelPatterns("cn/*")}
	if !AllowsModelAny(qualified, "cn/deepseek-v4.1-flash", "deepseek-v4.1-flash") {
		t.Fatal("canonical pattern rejected")
	}
	if AllowsModelAny(qualified, "codearts/deepseek-v4.1-flash", "deepseek-v4.1-flash") {
		t.Fatal("wrong exit accepted")
	}
	if !AllowsModelAny(Entry{}, "cn/anything") {
		t.Fatal("empty allow-list should remain unrestricted")
	}
}

func TestKeyAccountAllowlist(t *testing.T) {
	if !AllowsAccount(Entry{}, "anything") {
		t.Fatal("empty account list should be unrestricted")
	}
	entry := Entry{Accounts: []string{"uid-1", " uid-2 "}}
	if !AllowsAccount(entry, "uid-1") || !AllowsAccount(entry, "uid-2") {
		t.Fatal("exact uid rejected")
	}
	if AllowsAccount(entry, "uid-3") || AllowsAccount(entry, "uid-1*") {
		t.Fatal("non-exact uid accepted")
	}
	long := strings.Repeat("x", 129)
	refs := normalizeAccountRefs([]any{long, "ok"})
	if len(refs) != 1 || refs[0] != "ok" {
		t.Fatalf("length cap refs = %#v", refs)
	}
	many := make([]any, 0, 300)
	for i := 0; i < 300; i++ {
		many = append(many, string(rune('a'+i%26))+strings.Repeat("x", i%5)+string(rune('0'+i%10)))
	}
	refs = normalizeAccountRefs(many)
	if len(refs) > 200 {
		t.Fatalf("account cap = %d", len(refs))
	}
}

func TestKeyRealmBinding(t *testing.T) {
	entry := &Entry{Realm: " CN "}
	if got := BoundRealm(entry); got != "cn" {
		t.Fatalf("bound realm = %q", got)
	}
	if got := BoundRealm(&Entry{Realm: ""}); got != "" {
		t.Fatalf("unbound realm = %q", got)
	}
	if got := BoundRealm(nil); got != "" {
		t.Fatalf("nil realm = %q", got)
	}
}

func TestKeyConcurrency(t *testing.T) {
	limiter := NewLimiter()
	release1, ok := limiter.Acquire("key", 2)
	if !ok {
		t.Fatal("first acquire failed")
	}
	release2, ok := limiter.Acquire("key", 2)
	if !ok {
		t.Fatal("second acquire failed")
	}
	if _, ok := limiter.Acquire("key", 2); ok {
		t.Fatal("third acquire passed at limit")
	}
	release1()
	release1()
	if limiter.InFlight("key") != 1 {
		t.Fatalf("double release changed count: %d", limiter.InFlight("key"))
	}
	release3, ok := limiter.Acquire("key", 2)
	if !ok {
		t.Fatal("acquire after release failed")
	}
	release2()
	release3()
	if limiter.InFlight("key") != 0 {
		t.Fatalf("leaked slots: %d", limiter.InFlight("key"))
	}
	if _, ok := limiter.Acquire("unlimited", 0); !ok {
		t.Fatal("unlimited key blocked")
	}
	limiter.SetLimit("configured", 1)
	if _, ok := limiter.Acquire("configured"); !ok {
		t.Fatal("configured limit acquire failed")
	}
	if _, ok := limiter.Acquire("configured"); ok {
		t.Fatal("configured limit ignored")
	}

	// Concurrent acquire/release must not leak after many requests.
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := limiter.Acquire("busy", 4)
			if ok {
				release()
			}
		}()
	}
	wg.Wait()
	if got := limiter.InFlight("busy"); got != 0 {
		t.Fatalf("leaked after concurrent requests: %d", got)
	}
}
