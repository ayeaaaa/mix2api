// Package version exposes the mix2api build identity.
//
// Version, Commit, and Date are variables rather than constants so release
// builds can inject them with:
//
//	go build -ldflags "-X github.com/ayeaaaa/mix2api-v2/internal/core/version.Version=2.0.6 \
//	  -X github.com/ayeaaaa/mix2api-v2/internal/core/version.Commit=<sha> \
//	  -X github.com/ayeaaaa/mix2api-v2/internal/core/version.Date=<RFC3339>"
//
// An ordinary `go build` keeps the development defaults below.
package version

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// DefaultVersion is the v2.0.6 patch-release identity for this tree.
const DefaultVersion = "2.0.6"

// Version is the semantic version of this build, without a leading "v".
var Version = DefaultVersion

// Commit is the source revision used for the build. Release builds inject a
// full or abbreviated git SHA; local builds report "unknown".
var Commit = "unknown"

// Date is the build time in RFC3339 UTC. Release builds inject the commit
// timestamp (or SOURCE_DATE_EPOCH) so repeated builds of one revision match.
var Date = "unknown"

// Info is the machine-readable build identity exposed by `mix2api version`.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Current returns the runtime build identity.
func Current() Info {
	return Info{
		Version: normalize(Version),
		Commit:  normalize(Commit),
		Date:    normalize(Date),
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
}

// NormalizedVersion returns Version without a leading "v" or surrounding
// whitespace. It is the value used in filenames, manifests, and comparisons.
func NormalizedVersion() string { return normalize(Version) }

// JSON renders the build identity as one compact JSON line.
func (i Info) JSON() ([]byte, error) { return json.Marshal(i) }

// String renders the human-readable multi-line form printed by
// `mix2api version`. Blank or injected placeholder values stay visible so a
// bad release build cannot silently claim a clean identity.
func (i Info) String() string {
	return fmt.Sprintf("mix2api %s\ncommit: %s\nbuilt: %s\ngo: %s %s/%s\n",
		i.Version, i.Commit, i.Date, i.Go, i.OS, i.Arch)
}

// ParseDate returns the injected build time when it is valid RFC3339 and the
// zero time otherwise. Release tooling uses it to validate metadata.
func ParseDate(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func normalize(value string) string {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "v")
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}
