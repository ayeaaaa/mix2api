package providers

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// Registry is a thread-safe collection of providers keyed by realm.
//
// The zero value is ready to use. NewRegistry is provided for clarity.
type Registry struct {
	mu      sync.RWMutex
	byRealm map[string]Provider
	order   []Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byRealm: make(map[string]Provider)}
}

// Register adds one provider under its realm. It rejects nil providers, empty
// or blank realms, and realms that are already registered. Registration order
// is preserved for All.
func (r *Registry) Register(p Provider) error {
	if isNilProvider(p) {
		return errors.New("providers: cannot register a nil provider")
	}
	realm := p.Realm()
	if strings.TrimSpace(realm) == "" {
		return errors.New("providers: cannot register a provider with an empty realm")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byRealm == nil {
		r.byRealm = make(map[string]Provider)
	}
	if _, exists := r.byRealm[realm]; exists {
		return fmt.Errorf("providers: realm %q is already registered", realm)
	}
	r.byRealm[realm] = p
	r.order = append(r.order, p)
	return nil
}

// Get returns the provider registered for realm.
func (r *Registry) Get(realm string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byRealm[realm]
	return p, ok
}

// All returns the registered providers in registration order. The result is a
// copy; mutating it does not affect the registry.
func (r *Registry) All() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, len(r.order))
	copy(out, r.order)
	return out
}

// isNilProvider reports whether p is nil, including a typed nil pointer stored
// in the Provider interface. A typed nil would otherwise be accepted and then
// panic on the first method call.
func isNilProvider(p Provider) bool {
	if p == nil {
		return true
	}
	value := reflect.ValueOf(p)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
