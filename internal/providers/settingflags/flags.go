// Package settingflags holds small helpers for reading boolean settings whose
// v2 default is true. A missing key means the default.
package settingflags

// DefaultTrue reports the boolean value of key when explicitly stored, or the
// default (true) when absent or of the wrong type.
func DefaultTrue(settings map[string]any, key string) bool {
	if settings == nil {
		return true
	}
	value, ok := settings[key].(bool)
	if !ok {
		return true
	}
	return value
}

// DefaultFalse reports the boolean value of key when explicitly stored, or
// false when absent or of the wrong type.
func DefaultFalse(settings map[string]any, key string) bool {
	if settings == nil {
		return false
	}
	value, _ := settings[key].(bool)
	return value
}
