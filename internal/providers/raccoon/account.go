package raccoon

import "github.com/ayeaaaa/mix2api-v2/internal/core/accountstate"

// Account is one Raccoon account's credentials and generic runtime state.
// The pool, credits, and guards are layered on this type by later tasks.
type Account struct {
	Auth  Auth
	State accountstate.State
	Path  string
}
