package workbuddy

import (
	"os"
	"testing"
)

// TestMain disables the web human-pause by default. The production default is
// 2–8 seconds, but dozens of daily-web tests must not spend minutes sleeping.
func TestMain(m *testing.M) {
	_ = os.Setenv("WB_WEB_HUMAN_DELAY_MIN", "0")
	_ = os.Setenv("WB_WEB_HUMAN_DELAY_MAX", "0")
	// Alias tests opt in explicitly; legacy tests use the logical base id.
	_ = os.Setenv("WB_INTL_MODEL_ALIAS", "0")
	os.Exit(m.Run())
}
