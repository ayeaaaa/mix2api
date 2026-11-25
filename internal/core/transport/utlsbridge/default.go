package utlsbridge

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

var (
	defaultMu          sync.Mutex
	defaultInitialized bool
	defaultBridge      *Bridge
	defaultErr         error

	codeartsMu          sync.Mutex
	codeartsInitialized bool
	codeartsBridge      *Bridge
	codeartsErr         error
)

// Default returns the process-wide bridge selected by
// MIX2API_TLS_FINGERPRINT (or WB_TLS_FINGERPRINT). An unset value keeps the
// bridge off; "node" starts it once. Invalid values return an error instead of
// silently falling back to Go's native TLS.
func Default() (*Bridge, error) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultInitialized {
		return defaultBridge, defaultErr
	}
	defaultInitialized = true

	mode := strings.ToLower(strings.TrimSpace(os.Getenv("MIX2API_TLS_FINGERPRINT")))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv("WB_TLS_FINGERPRINT")))
	}
	switch mode {
	case "", "python", "go", "native", "stdlib", "0", "off", "false", "no":
	case "node":
		if strings.TrimSpace(os.Getenv("WB_CA_BUNDLE")) != "" {
			defaultErr = fmt.Errorf("utlsbridge: WB_CA_BUNDLE is not supported; use SSL_CERT_FILE or disable the bridge")
		} else {
			defaultBridge, defaultErr = NewWithConfig(Config{})
		}
	default:
		defaultErr = fmt.Errorf("utlsbridge: MIX2API_TLS_FINGERPRINT must be node or python, got %q", mode)
	}
	return defaultBridge, defaultErr
}

// CloseDefault stops the process-wide bridge, if it was started, and allows a
// later Default call to start a fresh bridge.
func CloseDefault() error {
	defaultMu.Lock()
	bridge := defaultBridge
	defaultBridge = nil
	defaultErr = nil
	defaultInitialized = false
	defaultMu.Unlock()
	if bridge == nil {
		return nil
	}
	return bridge.Close()
}

// CodeArtsDefault returns the process-wide CodeArts Agent uTLS bridge. It is
// separate from Default because the CodeArts ClientHello adds padding(21) and
// must not change the WorkBuddy profile.
func CodeArtsDefault() (*Bridge, error) {
	codeartsMu.Lock()
	defer codeartsMu.Unlock()
	if codeartsInitialized {
		return codeartsBridge, codeartsErr
	}
	codeartsInitialized = true
	codeartsBridge, codeartsErr = NewWithConfig(Config{Profile: "codearts"})
	return codeartsBridge, codeartsErr
}

// CloseCodeArtsDefault stops the CodeArts bridge and allows a later call to
// start a fresh one.
func CloseCodeArtsDefault() error {
	codeartsMu.Lock()
	bridge := codeartsBridge
	codeartsBridge = nil
	codeartsErr = nil
	codeartsInitialized = false
	codeartsMu.Unlock()
	if bridge == nil {
		return nil
	}
	return bridge.Close()
}
