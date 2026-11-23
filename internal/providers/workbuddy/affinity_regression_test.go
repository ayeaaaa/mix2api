package workbuddy

import (
	"context"
	"fmt"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
	"net/http"
	"testing"
)

func TestAffinityBillingMetadataDoesNotRebind(t *testing.T) {
	dir := t.TempDir()
	for _, uid := range []string{"a", "b"} {
		writeChatAccount(t, dir, uid, realmIntl, nil)
	}
	p := setupChatProvider(t, realmIntl, dir, nil)
	fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	p.chatBaseURL = newChatTestServer(t, fake).URL
	var hashes []string
	for _, cch := range []string{"aaa", "bbb"} {
		r := chatRequest(t, "deepseek-v4.1-flash", map[string]any{"model": "deepseek-v4.1-flash", "messages": []any{map[string]any{"role": "system", "content": fmt.Sprintf("x-anthropic-billing-header: cch=%s;\nKeep rules.", cch)}, map[string]any{"role": "user", "content": "first"}}})
		s, err := p.OpenChat(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, s.(providers.UsageSnapshotReporter).UsageSnapshot().Affinity.PrefixSHA)
		drainChatFrames(t, s)
		_ = s.Close()
	}
	calls := fake.snapshot()
	if hashes[0] != hashes[1] || calls[0].Header.Get("Authorization") != calls[1].Header.Get("Authorization") {
		t.Fatalf("sanitized metadata caused rebind: %v", hashes)
	}
}

func TestAffinityRealmFallbackRetainsDerivedPage(t *testing.T) {
	dir := t.TempDir()
	writePoolSettings(t, dir, map[string]any{"affinity_max_messages": 1, "workbuddy": map[string]any{"intl": map[string]any{"realm_fallback_enabled": true}}})
	writeChatAccount(t, dir, "intl-a", realmIntl, nil)
	for _, uid := range []string{"cn-a", "cn-b", "cn-c", "cn-d", "cn-e", "cn-f"} {
		writeChatAccount(t, dir, uid, realmCN, nil)
	}
	intl, _ := newPoolProvider(t, realmIntl, dir)
	cn, _ := newPoolProvider(t, realmCN, dir)
	intl.SetPeer(cn)
	intl.chatBaseURL = newChatTestServer(t, &chatTestFake{status: http.StatusTooManyRequests, body: `{"error":{"message":"usage exceeds frequency limit"}}`}).URL
	fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	cn.chatBaseURL = newChatTestServer(t, fake).URL
	for i := 0; i < 4; i++ {
		r := chatRequest(t, "deepseek-v4.1-flash", map[string]any{"model": "deepseek-v4.1-flash", "messages": []any{map[string]any{"role": "user", "content": "first"}, map[string]any{"role": "assistant", "content": "answer"}}})
		s, err := intl.OpenChat(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		d := s.(providers.UsageSnapshotReporter).UsageSnapshot().Affinity
		drainChatFrames(t, s)
		_ = s.Close()
		if d.Mode != "page" || d.PoolSize != 2 || d.PrefixSHA == "" {
			t.Errorf("fallback lost provenance: %+v", d)
		}
	}
	uids := map[string]bool{}
	for _, call := range fake.snapshot() {
		uids[call.Header.Get("Authorization")] = true
	}
	if len(uids) != 2 {
		t.Fatalf("fallback did not rotate within its V1 partition: %v", uids)
	}
}

func TestAffinityModelCohortsDoNotResetEachOther(t *testing.T) {
	dir := t.TempDir()
	uids := []string{}
	for i := 0; i < 40; i++ {
		uid := fmt.Sprintf("u%02d", i)
		uids = append(uids, uid)
		writeChatAccount(t, dir, uid, realmIntl, nil)
	}
	p := setupChatProvider(t, realmIntl, dir, nil).Pool()
	o := providers.LoadAffinityOptions(map[string]any{"affinity_model_pool_sizes": map[string]any{"intl/deepseek-v4.1-flash-sg": 3, "intl/glm-5.3-flash": 3}})
	selected := map[string]bool{}
	for i := 0; i < 3; i++ {
		for _, model := range []string{"deepseek-v4.1-flash-sg", "glm-5.3-flash"} {
			r := providers.AffinityRoute{Mode: "prefix", Key: fmt.Sprintf("%s-new-%d", model, i)}
			a, err := p.Pick(model, r.Key, nil, r.Policy(uids, realmIntl, model, o))
			if err != nil {
				t.Fatal(err)
			}
			if model == "deepseek-v4.1-flash-sg" {
				selected[a.UID()] = true
			}
		}
	}
	if len(selected) != 3 {
		t.Fatalf("other model reset cohort rotation: %v", selected)
	}
}

func TestAffinityWeightedStateBoundedAcrossConfigChurn(t *testing.T) {
	dir := t.TempDir()
	for _, uid := range []string{"a", "b"} {
		writeChatAccount(t, dir, uid, realmIntl, nil)
	}
	p := setupChatProvider(t, realmIntl, dir, nil).Pool()
	eligible := p.Accounts()
	p.pickFallback(eligible, "active")
	for i := 0; i < 767; i++ {
		p.pickFallback(eligible, fmt.Sprintf("scope-%d", i))
	}
	if a := p.pickFallback(eligible, "active"); a.UID() != "b" {
		t.Fatal("active progress lost")
	}
	p.pickFallback(eligible, "new")
	if a := p.pickFallback(eligible, "active"); a.UID() != "a" {
		t.Fatal("recent scope evicted")
	}
	total := 0
	for _, state := range p.weightStates {
		total += len(state)
	}
	if total > 768*len(eligible) {
		t.Fatalf("configuration churn retains unbounded weighted states: %d", total)
	}
}

func TestV1PageRunsPrioritiesBeforeCursor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
	}{{"manual", map[string]any{"weight": 10}}, {"expiring", map[string]any{"credits": poolCredits(1)}}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeChatAccount(t, dir, "a", realmIntl, tc.fields)
			writeChatAccount(t, dir, "b", realmIntl, map[string]any{"credits": poolCredits(30)})
			p := setupChatProvider(t, realmIntl, dir, nil).Pool()
			setAccountWindow(t, p, 7)
			policy := providers.AffinityPolicy{Cohort: []string{"b", "a"}, Rotate: true, Scope: "page/intl/0/3"}
			for i := 0; i < 3; i++ {
				a, err := p.Pick("m", "", nil, policy)
				if err != nil || a.UID() != "a" {
					t.Fatalf("%s priority bypassed: %v %v", tc.name, a, err)
				}
			}
		})
	}
}

func TestV1SmallPoolLongConversationStaysSticky(t *testing.T) {
	dir := t.TempDir()
	for _, uid := range []string{"a", "b", "c"} {
		writeChatAccount(t, dir, uid, realmIntl, nil)
	}
	p := setupChatProvider(t, realmIntl, dir, map[string]any{"affinity_max_messages": 1})
	fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	p.chatBaseURL = newChatTestServer(t, fake).URL
	for i := 0; i < 3; i++ {
		r := chatRequest(t, "m", map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "first"}, map[string]any{"role": "assistant", "content": "history"}}})
		s, err := p.OpenChat(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		if d := s.(providers.UsageSnapshotReporter).UsageSnapshot().Affinity; d.Mode != "prefix" {
			t.Fatalf("small realm didn't restore affinity: %+v", d)
		}
		drainChatFrames(t, s)
		_ = s.Close()
	}
	calls := fake.snapshot()
	for _, call := range calls {
		if call.Header.Get("Authorization") != calls[0].Header.Get("Authorization") {
			t.Fatal("small realm rotated despite V1 sticky guard")
		}
	}
}

func TestV1PageFallbackDoesNotCreateStickyBinding(t *testing.T) {
	dir := t.TempDir()
	for _, uid := range []string{"a", "b", "c", "d"} {
		writeChatAccount(t, dir, uid, realmIntl, nil)
	}
	p := setupChatProvider(t, realmIntl, dir, nil).Pool()
	policy := providers.AffinityPolicy{Cohort: []string{"a", "b"}, Rotate: true, Scope: "page/intl/0/3"}
	a, err := p.Pick("m", "long", map[string]bool{"a": true, "b": true}, policy)
	if err != nil || (a.UID() != "c" && a.UID() != "d") {
		t.Fatal("no legal group-external fallback")
	}
	if len(p.affinity.recent("long")) != 0 {
		t.Fatal("page fallback pinned the conversation")
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		a, err := p.Pick("m", "long", nil, policy)
		if err != nil {
			t.Fatal(err)
		}
		seen[a.UID()] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("returned page didn't rotate: %v", seen)
	}
}

func TestV1PageWeightedSharesSpanModels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
	}{{"manual", map[string]any{"weight": 10}}, {"expiring", map[string]any{"credits": poolCredits(1)}}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, uid := range []string{"a", "d"} {
				writeChatAccount(t, dir, uid, realmIntl, tc.fields)
			}
			p := setupChatProvider(t, realmIntl, dir, nil).Pool()
			setAccountWindow(t, p, 7)
			policy := providers.AffinityPolicy{Cohort: []string{"a", "d"}, Rotate: true, Scope: "page/intl/0/3"}
			for i, want := range []string{"a", "d", "a", "d"} {
				model := []string{"m", "n"}[i%2]
				a, err := p.Pick(model, "", nil, policy)
				if err != nil || a.UID() != want {
					t.Fatalf("V1 shared %s priority: turn %d got %v", tc.name, i, err)
				}
			}
		})
	}
}
