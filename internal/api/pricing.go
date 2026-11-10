package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/pricing"
)

// PricingHandlers owns the panel pricing status and runtime mapping/refresh
// controls. It never fetches a price on GET.
type PricingHandlers struct {
	Snapshot  *pricing.Snapshot
	Refresher *pricing.Refresher
	Gate      func(http.Handler) http.Handler

	RefreshMinutes func() float64
	VariantInherit func() bool
	OverridesFile  string
	PoliciesFile   string
	TimelineFile   string
}

// NewPricingHandlers builds the pricing route group.
func NewPricingHandlers(snapshot *pricing.Snapshot, refresher *pricing.Refresher, gate func(http.Handler) http.Handler) *PricingHandlers {
	return &PricingHandlers{Snapshot: snapshot, Refresher: refresher, Gate: gate}
}

// Register mounts the v1 pricing routes.
func (h *PricingHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	get := func(path string, handler http.HandlerFunc) {
		var wrapped http.Handler = handler
		if h.Gate != nil {
			wrapped = h.Gate(wrapped)
		}
		mux.Handle(path, methodGET(wrapped))
	}
	post := func(path string, handler http.HandlerFunc) {
		var wrapped http.Handler = handler
		if h.Gate != nil {
			wrapped = h.Gate(wrapped)
		}
		mux.Handle(path, methodPOST(wrapped))
	}
	get("/pricing", h.handleStatus)
	post("/pricing/refresh", h.handleRefresh)
	post("/pricing/mapping", h.handleMapping)
}

func (h *PricingHandlers) handleStatus(w http.ResponseWriter, r *http.Request) {
	_ = r
	view := h.status()
	writeJSONResponse(w, http.StatusOK, view)
}

func (h *PricingHandlers) status() map[string]any {
	interval := 0.0
	if h.RefreshMinutes != nil {
		interval = pricing.ClampRefreshMinutes(h.RefreshMinutes())
	}
	variant := true
	if h.VariantInherit != nil {
		variant = h.VariantInherit()
	}
	view := map[string]any{
		"interval_minutes": interval,
		"enabled":          h.Snapshot != nil && h.Snapshot.Enabled(),
		"running":          false,
		"master_enabled":   h.Snapshot != nil && h.Snapshot.Enabled(),
		"policies":         0,
		"models":           0,
		"current":          map[string]any{},
		"logs":             []any{},
		"gaps":             []any{},
		"gap_summary": map[string]any{
			"total": 0, "or_missing": 0, "variant_unmatched": 0,
			"aliases": 0, "variants_enabled": variant,
		},
		"variant_inherit": variant,
		"overrides_file":  h.OverridesFile,
		"policies_file":   h.PoliciesFile,
		"timeline":        h.TimelineFile,
		"msg":             "",
	}
	if h.Refresher != nil {
		for key, value := range h.Refresher.Status() {
			view[key] = value
		}
	}
	if h.Snapshot == nil {
		view["enabled"] = false
		view["master_enabled"] = false
		view["msg"] = "未运行"
		return view
	}
	view["policies"] = len(h.Snapshot.Policies())
	view["models"] = len(h.Snapshot.Models)
	view["timeline"] = h.TimelineFile
	if len(h.Snapshot.Models) == 0 {
		view["msg"] = "未运行"
	}
	return view
}

func (h *PricingHandlers) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if h.Refresher == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "pricing refresher unavailable")
		return
	}
	if err := h.Refresher.RunOnce(r.Context()); err != nil {
		// A disabled or already-running refresh is not a server failure; the
		// panel reads the same status either way.
		if strings.Contains(err.Error(), "busy") || strings.Contains(err.Error(), "disabled") {
			writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "started": false, "status": h.status()})
			return
		}
		writeV1Error(w, http.StatusBadGateway, "server_error", "pricing refresh failed")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "started": true, "status": h.status()})
}

func (h *PricingHandlers) handleMapping(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	model := strings.TrimSpace(textField(payload["model"]))
	openrouter := strings.TrimSpace(textField(payload["openrouter_id"]))
	if model == "" {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if h.Snapshot == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "pricing snapshot unavailable")
		return
	}
	if err := h.Snapshot.SaveRuntimeOverride(model, openrouter); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "model": model, "openrouter_id": openrouter})
}
