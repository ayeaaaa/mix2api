package panel

// Status builds the public /panel/status payload. Callbacks keep the panel
// package independent of API-key and provider implementations.
type Status struct {
	Password  *Password
	Sessions  *Sessions
	APIKeySet func() bool
	Raccoon   func() map[string]any
}

// View returns the login-screen state. token is the optional X-Panel-Token.
func (s *Status) View(token string) map[string]any {
	out := map[string]any{
		"panel_password_required":   true,
		"panel_password_is_default": true,
		"authenticated":             false,
		"crypto_ok":                 true,
	}
	if s != nil && s.Password != nil {
		out["panel_password_is_default"] = s.Password.IsDefault()
		ok, reason := s.Password.CryptoAvailable()
		out["crypto_ok"] = ok
		if !ok {
			out["crypto_error"] = reason
		}
	}
	if s != nil && s.Sessions != nil {
		out["authenticated"] = s.Sessions.Valid(token)
	}
	apiKeySet := false
	if s != nil && s.APIKeySet != nil {
		apiKeySet = s.APIKeySet()
	}
	out["api_key_set"] = apiKeySet
	if s != nil && s.Raccoon != nil {
		out["raccoon"] = s.Raccoon()
	}
	return out
}
