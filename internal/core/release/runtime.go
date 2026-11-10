package release

import "runtime/debug"

// RuntimeGOARM reports the GOARM value embedded in this binary's build info.
// Go does not expose GOARM at runtime elsewhere. Non-ARM builds and binaries
// without build info return an empty string.
func RuntimeGOARM() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "GOARM" {
			return setting.Value
		}
	}
	return ""
}
