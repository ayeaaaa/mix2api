package api

import "net/http"

// passThroughAuth is an explicit test-only authorization decision. Production
// defaults fail closed; these helpers opt tests into the routes they exercise.
func passThroughAuth(next http.Handler) http.Handler { return next }

func newTestHTTPServer(opts ServerOptions) *http.Server {
	if opts.APIKeyAuth == nil {
		opts.APIKeyAuth = passThroughAuth
	}
	if opts.PanelAuth == nil {
		opts.PanelAuth = passThroughAuth
	}
	return NewServer(opts)
}

func newTestServer(opts ServerOptions) *Server {
	if opts.APIKeyAuth == nil {
		opts.APIKeyAuth = passThroughAuth
	}
	if opts.PanelAuth == nil {
		opts.PanelAuth = passThroughAuth
	}
	return New(opts)
}
