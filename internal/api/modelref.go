package api

import (
	"fmt"
	"strings"
)

// modelRef is a parsed model reference. A registered provider realm followed
// by "/" is an exit; every other prefix is part of a bare upstream model ID.
type modelRef struct {
	Raw       string
	Exit      string
	Upstream  string
	Canonical string
	Qualified bool
}

// parseModelRef splits raw at the first slash. The candidate exit is matched
// case-insensitively against registered exits; the upstream segment is
// preserved verbatim. Unknown prefixes are treated as bare model IDs. The only
// parse error is a registered exit with an empty upstream.
func parseModelRef(raw string, isExit func(string) bool) (modelRef, error) {
	trimmed := strings.TrimSpace(raw)
	ref := modelRef{
		Raw:       trimmed,
		Upstream:  trimmed,
		Canonical: trimmed,
	}
	head, tail, ok := strings.Cut(trimmed, "/")
	if !ok {
		return ref, nil
	}
	exit := strings.ToLower(head)
	if !isExit(exit) {
		return ref, nil
	}
	ref.Exit = exit
	ref.Upstream = tail
	ref.Canonical = canonicalModelID(exit, tail)
	ref.Qualified = true
	if tail == "" {
		return ref, fmt.Errorf("model %q has no upstream model after exit %q", trimmed, exit)
	}
	return ref, nil
}

// canonicalModelID renders a qualified model ID with a lowercase exit.
func canonicalModelID(exit, upstream string) string {
	return strings.ToLower(exit) + "/" + upstream
}

// Requested returns the model ID the client asked for.
func (r modelRef) Requested() string {
	if r.Qualified {
		return r.Canonical
	}
	return r.Raw
}
