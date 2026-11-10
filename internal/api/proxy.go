package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/proxy"
)

// ProxyHandlers owns the panel proxy-slot routes.
type ProxyHandlers struct {
	Service  *proxy.Slots
	Gate     func(http.Handler) http.Handler
	Probe    func(ctx context.Context, url string) (map[string]any, error)
	Discover func(ctx context.Context) []map[string]any
}

// NewProxyHandlers builds the proxy route group.
func NewProxyHandlers(service *proxy.Slots, gate func(http.Handler) http.Handler, probe func(context.Context, string) (map[string]any, error), discover func(context.Context) []map[string]any) *ProxyHandlers {
	return &ProxyHandlers{Service: service, Gate: gate, Probe: probe, Discover: discover}
}

// Register mounts the v1 proxy routes.
func (h *ProxyHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil || h.Service == nil {
		return
	}
	routes := []struct {
		path   string
		method string
		handle http.HandlerFunc
	}{
		{"/proxy/slots", http.MethodGet, h.handleList},
		{"/proxy/slots/save", http.MethodPost, h.handleSave},
		{"/proxy/slots/test", http.MethodPost, h.handleTest},
		{"/proxy/discover", http.MethodPost, h.handleDiscover},
	}
	for _, route := range routes {
		var handler http.Handler = route.handle
		if h.Gate != nil {
			handler = h.Gate(handler)
		}
		if route.method == http.MethodPost {
			handler = methodPOST(handler)
		} else {
			handler = methodGET(handler)
		}
		mux.Handle(route.path, handler)
	}
}

func (h *ProxyHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	slots, err := h.Service.List()
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "proxy slots unavailable")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"slots": slots})
}

func (h *ProxyHandlers) handleSave(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	raw, ok := payload["slots"].([]any)
	if !ok {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "slots must be a list")
		return
	}
	incoming := make([]proxy.Slot, 0, len(raw))
	for _, item := range raw {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		incoming = append(incoming, slotFromMap(object))
	}
	slots, err := h.Service.Replace(incoming)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "proxy slots save failed")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"slots": slots})
}

func (h *ProxyHandlers) handleTest(w http.ResponseWriter, r *http.Request) {
	payload, ok := decodePanelObject(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(textField(payload["id"]))
	slots, err := h.Service.List()
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "proxy slots unavailable")
		return
	}
	var slot *proxy.Slot
	for i := range slots {
		if slots[i].ID == id {
			slot = &slots[i]
			break
		}
	}
	if slot == nil {
		writeV1Error(w, http.StatusNotFound, "invalid_request_error", "no such proxy slot")
		return
	}
	if h.Probe == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "proxy probe unavailable")
		return
	}
	result, err := h.Probe(r.Context(), slot.URL)
	if err != nil {
		writeV1Error(w, http.StatusBadGateway, "server_error", "proxy probe failed")
		return
	}
	if result == nil {
		result = map[string]any{}
	}
	reply := map[string]any{"id": id, "slot": nil}
	for key, value := range result {
		reply[key] = value
	}
	if okValue, _ := result["ok"].(bool); okValue {
		fields := map[string]any{"ip": textField(result["exit_ip"]), "probed_at": time.Now().Unix()}
		if intelOK, _ := result["intel_ok"].(bool); intelOK {
			fields["country"] = textField(result["country"])
			fields["country_code"] = textField(result["country_code"])
			fields["ip_type"] = textField(result["ip_type"])
			fields["isp"] = textField(result["isp"])
			fields["asn"] = textField(result["asn"])
		}
		if updated, found, updateErr := h.Service.Update(id, fields, nil); updateErr == nil && found {
			reply["slot"] = updated
			reply["name"] = updated.Name
		}
	}
	writeJSONResponse(w, http.StatusOK, reply)
}

func (h *ProxyHandlers) handleDiscover(w http.ResponseWriter, r *http.Request) {
	if h.Discover == nil {
		writeJSONResponse(w, http.StatusOK, map[string]any{"candidates": []map[string]any{}})
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"candidates": h.Discover(r.Context())})
}

func decodePanelObject(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return nil, false
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return payload, true
}

func slotFromMap(raw map[string]any) proxy.Slot {
	enabled := true
	if value, ok := raw["enabled"].(bool); ok {
		enabled = value
	}
	probedAt, _ := strconv.ParseInt(textField(raw["probed_at"]), 10, 64)
	return proxy.Slot{
		ID:          strings.TrimSpace(textField(raw["id"])),
		Name:        strings.TrimSpace(textField(raw["name"])),
		URL:         strings.TrimSpace(textField(raw["url"])),
		Enabled:     enabled,
		IP:          strings.TrimSpace(textField(raw["ip"])),
		Country:     strings.TrimSpace(textField(raw["country"])),
		CountryCode: strings.ToUpper(strings.TrimSpace(textField(raw["country_code"]))),
		IPType:      strings.ToLower(strings.TrimSpace(textField(raw["ip_type"]))),
		ISP:         strings.TrimSpace(textField(raw["isp"])),
		ASN:         strings.TrimSpace(textField(raw["asn"])),
		ProbedAt:    probedAt,
	}
}
