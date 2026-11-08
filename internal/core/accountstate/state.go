// Package accountstate defines provider-independent account runtime state and
// its JSON persistence.
package accountstate

import "time"

// State is the generic runtime state shared by provider accounts. Provider
// specific data belongs in the provider package, not here.
//
// The UID is the store key, so it is not repeated in each JSON entry.
type State struct {
	UID              string    `json:"-"`
	Disabled         bool      `json:"disabled"`
	ManualDisabled   bool      `json:"manual_disabled"`
	Reason           string    `json:"reason"`
	CoolUntil        time.Time `json:"cool_until"`
	LastUsed         time.Time `json:"last_used"`
	Note             string    `json:"note"`
	TasksEnabled     bool      `json:"tasks_enabled"`
	ActiveConcurrent int       `json:"active_concurrent"`
	MaxConcurrent    int       `json:"max_concurrent"`
}

// Healthy reports whether the account may serve a request at now. Cooling
// ends exactly at CoolUntil. A manual disable is expected to also set
// Disabled.
func (s State) Healthy(now time.Time) bool {
	return !s.Disabled && !s.CoolUntil.After(now)
}
