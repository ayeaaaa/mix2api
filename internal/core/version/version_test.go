package version

import (
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	if Version == "" || !strings.HasPrefix(Version, "2.") {
		t.Fatal(Version)
	}
}
