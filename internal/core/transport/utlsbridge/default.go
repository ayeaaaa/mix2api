package utlsbridge

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

var (
	defaultOnce   sync.Once
	defaultMu     sync.Mutex
	defaultBridge *Bridge
	defaultErr    error
)

// Default returns the process-wide bridge selected by
// MIX2API_TLS_FINGERPRINT (or WB_TLS_FINGERPRINT). An unset value keeps the
// bridge off; "node" starts it once. Invalid values return an error instead of
// silently falling back to Go's native TLS.
func Default() (*Bridge, error) {
	defaultOnce.Do(func() {
		mode := strings.ToLower(strings.TrimSpace(os.Getenv("MIX2API_TLS_FINGERPRINT")))
		if mode == "" {
			mode = strings.ToLower(strings.TrimSpace(os.Getenv("WB_TLS_FINGERPRINT")))
		}
		var bridge *Bridge
		var err error
		switch mode {
		case "", "python", "go", "native", "stdlib", "0", "off", "false", "no":
		case "node":
			if strings.TrimSpace(os.Getenv("WB_CA_BUNDLE")) != "" {
				err = fmt.Errorf("utlsbridge: WB_CA_BUNDLE is not supported; use SSL_CERT_FILE or disable the bridge")
			} else {
				bridge, err = NewWithConfig(Config{})
			}
		default:
			err = fmt.Errorf("utlsbridge: MIX2API_TLS_FINGERPRINT must be node or python, got %q", mode)
		}
		defaultMu.Lock()
		defaultBridge = bridge
		defaultErr = err
		defaultMu.Unlock()
	})
	defaultMu.Lock()
	defer defaultMu.Unlock()
	return defaultBridge, defaultErr
}

// CloseDefault stops the process-wide bridge, if it was started.
func CloseDefault() error {
	defaultMu.Lock()
	bridge := defaultBridge
	defaultMu.Unlock()
	if bridge == nil {
		return nil
	}
	err := bridge.Close()
	defaultMu.Lock()
	if defaultBridge == bridge {
		defaultBridge = nil
	}
	defaultMu.Unlock()
	return err
}
