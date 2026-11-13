package providers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
)

// fakeProvider is a minimal in-memory Provider used to exercise the registry.
type fakeProvider struct {
	id    string
	realm string
}

var _ Provider = fakeProvider{}

func (p fakeProvider) ID() string    { return p.id }
func (p fakeProvider) Realm() string { return p.realm }

func (p fakeProvider) Overview(context.Context, bool) (any, error)   { return nil, nil }
func (p fakeProvider) Health(context.Context, bool) (Health, error)  { return nil, nil }
func (p fakeProvider) Models(context.Context, bool) ([]Model, error) { return nil, nil }
func (p fakeProvider) Routes() []Route                               { return nil }
func (p fakeProvider) Jobs() []scheduler.Job                         { return nil }
func (p fakeProvider) OpenChat(context.Context, ChatRequest) (ChatStream, error) {
	return nil, errors.New("fakeProvider: OpenChat not implemented")
}

func TestRegistryRegisterAndDuplicate(t *testing.T) {
	r := NewRegistry()
	p := fakeProvider{id: "raccoon", realm: "raccoon"}
	if err := r.Register(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("raccoon"); !ok {
		t.Fatal("missing")
	}
	if err := r.Register(p); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestRegistryRejectsEmptyRealm(t *testing.T) {
	r := NewRegistry()
	for _, realm := range []string{"", "   ", "\t\n"} {
		err := r.Register(fakeProvider{id: "empty", realm: realm})
		if err == nil {
			t.Fatalf("realm %q accepted", realm)
		}
	}
	if got := len(r.All()); got != 0 {
		t.Fatalf("All() length = %d, want 0", got)
	}
}

func TestRegistryRejectsNilProvider(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("nil provider accepted")
	}

	var typedNil *fakeProvider
	if err := r.Register(typedNil); err == nil {
		t.Fatal("typed nil provider accepted")
	}
	if got := len(r.All()); got != 0 {
		t.Fatalf("All() length = %d, want 0", got)
	}
}

func TestRegistryGetUnknown(t *testing.T) {
	r := NewRegistry()
	if p, ok := r.Get("nope"); ok || p != nil {
		t.Fatalf("Get(nope) = (%v, %v), want (nil, false)", p, ok)
	}
}

func TestRegistryAllPreservesRegistrationOrder(t *testing.T) {
	r := NewRegistry()
	want := []string{"alpha", "beta", "gamma"}
	for _, realm := range want {
		if err := r.Register(fakeProvider{id: realm + "-id", realm: realm}); err != nil {
			t.Fatalf("Register(%q): %v", realm, err)
		}
	}

	got := r.All()
	if len(got) != len(want) {
		t.Fatalf("All() length = %d, want %d", len(got), len(want))
	}
	for i, p := range got {
		if p.Realm() != want[i] {
			t.Fatalf("All()[%d].Realm() = %q, want %q", i, p.Realm(), want[i])
		}
	}
}

func TestRegistryDuplicatePreservesOriginal(t *testing.T) {
	r := NewRegistry()
	original := fakeProvider{id: "original", realm: "shared"}
	duplicate := fakeProvider{id: "duplicate", realm: "shared"}
	if err := r.Register(original); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(duplicate); err == nil {
		t.Fatal("duplicate accepted")
	}

	p, ok := r.Get("shared")
	if !ok {
		t.Fatal("Get(shared) missing")
	}
	if p.ID() != original.id {
		t.Fatalf("Get(shared).ID() = %q, want %q", p.ID(), original.id)
	}
	all := r.All()
	if len(all) != 1 || all[0].ID() != original.id {
		t.Fatalf("All() = %v, want only %q", all, original.id)
	}
}

func TestRegistryWorkBuddyRealmsAreDistinct(t *testing.T) {
	r := NewRegistry()
	intl := fakeProvider{id: "workbuddy", realm: "intl"}
	cn := fakeProvider{id: "workbuddy", realm: "cn"}
	if err := r.Register(intl); err != nil {
		t.Fatalf("Register(intl): %v", err)
	}
	if err := r.Register(cn); err != nil {
		t.Fatalf("Register(cn): %v", err)
	}

	gotIntl, ok := r.Get("intl")
	if !ok || gotIntl.ID() != "workbuddy" {
		t.Fatalf("Get(intl) = (%v, %v)", gotIntl, ok)
	}
	gotCn, ok := r.Get("cn")
	if !ok || gotCn.ID() != "workbuddy" {
		t.Fatalf("Get(cn) = (%v, %v)", gotCn, ok)
	}
	if len(r.All()) != 2 {
		t.Fatalf("All() length = %d, want 2", len(r.All()))
	}
}

func TestRegistryAllReturnsSnapshot(t *testing.T) {
	r := NewRegistry()
	original := fakeProvider{id: "raccoon", realm: "raccoon"}
	if err := r.Register(original); err != nil {
		t.Fatal(err)
	}

	snapshot := r.All()
	snapshot[0] = fakeProvider{id: "mutated", realm: "raccoon"}
	_ = append(snapshot, fakeProvider{id: "extra", realm: "extra"})

	p, ok := r.Get("raccoon")
	if !ok {
		t.Fatal("Get(raccoon) missing")
	}
	if p.ID() != original.id {
		t.Fatalf("Get(raccoon).ID() = %q, want %q", p.ID(), original.id)
	}
	if got := r.All(); len(got) != 1 || got[0].ID() != original.id {
		t.Fatalf("All() = %v, want only %q", got, original.id)
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	const workers = 64

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			realm := fmt.Sprintf("realm-%02d", i)
			if err := r.Register(fakeProvider{id: realm, realm: realm}); err != nil {
				t.Errorf("Register(%q): %v", realm, err)
			}
			if _, ok := r.Get(realm); !ok {
				t.Errorf("Get(%q) missing", realm)
			}
			_ = r.All()
		}()
	}
	wg.Wait()

	if got := len(r.All()); got != workers {
		t.Fatalf("All() length = %d, want %d", got, workers)
	}
}
