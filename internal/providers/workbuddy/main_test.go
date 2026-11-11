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
	os.Exit(m.Run())
}
