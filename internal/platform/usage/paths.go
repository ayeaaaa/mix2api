// Package usage owns the append-only usage ledger and the read paths that the
// panel and internal tooling use to page and export it. It preserves the
// v1.6.5 JSONL format: one compact JSON object per line, unknown fields kept,
// and no legacy usage-summary.json writes.
package usage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	usageLogName      = "usage.jsonl"
	limitEventsName   = "limit-events.jsonl"
	estimateDailyName = "estimate-daily.json"
	recentIndexName   = "usage-recent-index.json"
)

// Paths are the v1.6.5 usage data files resolved under one directory.
type Paths struct {
	Dir           string
	Log           string // usage.jsonl
	LimitEvents   string // limit-events.jsonl
	EstimateDaily string // estimate-daily.json
	RecentIndex   string // usage-recent-index.json
}

// ResolvePaths resolves the usage directory once at startup. Precedence is the
// explicit usageDir argument, USAGE_DIR, WB_PROXY_USAGE_DIR, then
// <working directory>/usage. accountsDir is deliberately separate: this
// package never creates or writes the accounts directory.
func ResolvePaths(accountsDir, usageDir string) (Paths, error) {
	_ = accountsDir

	dir := usageDir
	if dir == "" {
		dir = os.Getenv("USAGE_DIR")
	}
	if dir == "" {
		dir = os.Getenv("WB_PROXY_USAGE_DIR")
	}
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return Paths{}, fmt.Errorf("usage: resolve working directory: %w", err)
		}
		dir = filepath.Join(cwd, "usage")
	}
	return pathsFor(filepath.Clean(dir)), nil
}

func pathsFor(dir string) Paths {
	return Paths{
		Dir:           dir,
		Log:           filepath.Join(dir, usageLogName),
		LimitEvents:   filepath.Join(dir, limitEventsName),
		EstimateDaily: filepath.Join(dir, estimateDailyName),
		RecentIndex:   filepath.Join(dir, recentIndexName),
	}
}

// EnsureDir creates the usage directory with the v1-compatible 0755 mode.
func (p Paths) EnsureDir() error {
	if p.Dir == "" {
		return errors.New("usage: empty usage directory")
	}
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return fmt.Errorf("usage: create directory %s: %w", p.Dir, err)
	}
	return nil
}
