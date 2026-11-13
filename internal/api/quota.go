package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/probes"
)

// QuotaHandlers owns the panel-only forced quota scan.
type QuotaHandlers struct {
	Service *probes.Service
	Gate    func(http.Handler) http.Handler
}

// NewQuotaHandlers builds the quota route group.
func NewQuotaHandlers(service *probes.Service, gate func(http.Handler) http.Handler) *QuotaHandlers {
	return &QuotaHandlers{Service: service, Gate: gate}
}

// Register mounts POST /quota-scan/run behind the panel gate.
func (h *QuotaHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil || h.Service == nil {
		return
	}
	var handler http.Handler = http.HandlerFunc(h.handleRun)
	if h.Gate != nil {
		handler = h.Gate(handler)
	}
	mux.Handle("/quota-scan/run", methodPOST(handler))
}

func (h *QuotaHandlers) handleRun(w http.ResponseWriter, r *http.Request) {
	// The v1 panel button posts an empty JSON object. Drain it so the next
	// request on a keep-alive connection starts on a clean request line.
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
	}
	sent, err := h.Service.RunQuota(r.Context(), true)
	if err != nil {
		if errors.Is(err, probes.ErrNoProvider) {
			writeV1Error(w, http.StatusServiceUnavailable, "server_error", "account pool unavailable")
			return
		}
		writeV1Error(w, http.StatusInternalServerError, "server_error", "quota scan failed")
		return
	}
	msg := "没有需要查询的配额线（未勾选模型或都已到下一次查询时刻）"
	if sent > 0 {
		msg = "已查询 " + itoaAPI(sent) + " 条配额线"
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "sent": sent, "msg": msg})
}

func itoaAPI(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
