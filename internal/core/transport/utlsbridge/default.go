package utlsbridge

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

var (
	defaultOnce   sync.Once
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
		switch mode {
		case "", "python", "go", "native", "stdlib", "0", "off", "false", "no":
			return
		case "node":
			if strings.TrimSpace(os.Getenv("WB_CA_BUNDLE")) != "" {
				defaultErr = fmt.Errorf("utlsbridge: WB_CA_BUNDLE is not supported; unset it or use MIX2API_TLS_FINGERPRINT=python")
				return
			}
			defaultBridge, defaultErr = NewWithConfig(Config{})
		default:
			defaultErr = fmt.Errorf("utlsbridge: MIX2API_TLS_FINGERPRINT must be node or python, got %q", mode)
		}
	})
	return defaultBridge, defaultErr
}

// CloseDefault stops the process-wide bridge, if it was started.
func CloseDefault() error {
	if defaultBridge == nil {
		return nil
	}
	return defaultBridge.Close()
}
