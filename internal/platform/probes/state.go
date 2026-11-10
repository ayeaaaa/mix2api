// Package probes schedules model-window and quota-scan probes through optional
// provider hooks. The platform owns state and timing; providers own auth.
package probes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	WindowStateFile = "model_windows.json"
	QuotaStateFile  = "quota_scan.json"
	StateVersion    = 1

	WindowMaxAttempts    = 20
	WindowRetryDelay     = 15 * time.Second
	WindowMaxAge         = 7 * 24 * time.Hour
	WindowBatchLimit     = 8
	WindowFreshThreshold = 30 * time.Second
	WindowDefaultGap     = 400 * time.Millisecond

	QuotaInterval      = 6 * time.Hour
	QuotaRetryDelay    = 15 * time.Minute
	QuotaMaxAttempts   = 6
	QuotaFirstDelay    = 5 * time.Second
	QuotaFirstStagger  = 15 * time.Second
	QuotaFirstDelayMax = time.Hour
	QuotaBatchLimit    = 6
	QuotaMaxAge        = 30 * 24 * time.Hour
	QuotaDefaultGap    = 400 * time.Millisecond
)

func atomicWriteJSON(path string, payload any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	remove := func() { _ = os.Remove(name) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		remove()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		remove()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		remove()
		return err
	}
	if err := tmp.Close(); err != nil {
		remove()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		remove()
		return err
	}
	return nil
}

// State is the shared storage root for both scheduler families.
type State struct {
	Dir    string
	Window *WindowState
	Quota  *QuotaState
}

// NewState opens both state files under accountsDir.
func NewState(accountsDir string) *State {
	return &State{
		Dir:    accountsDir,
		Window: NewWindowState(accountsDir),
		Quota:  NewQuotaState(accountsDir),
	}
}
