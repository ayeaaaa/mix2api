package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/agents"
)

const agentsMaxBodyBytes = 1 << 20

// AgentHandlers owns the local agent configurator routes.
type AgentHandlers struct {
	Service *agents.Service
	Server  *Server
	Gate    func(http.Handler) http.Handler
}

// NewAgentHandlers builds the agent route group.
func NewAgentHandlers(service *agents.Service, gate func(http.Handler) http.Handler) *AgentHandlers {
	return &AgentHandlers{Service: service, Gate: gate}
}

// Register mounts the six v1 agent routes. /agents/cli is public.
func (h *AgentHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil || h.Service == nil {
		return
	}
	gated := func(next http.HandlerFunc) http.Handler {
		var handler http.Handler = next
		if h.Gate != nil {
			handler = h.Gate(handler)
		}
		return handler
	}
	mux.Handle("/agents", methodGET(gated(h.handleOverview)))
	mux.Handle("/agents/available", methodGET(gated(h.handleAvailable)))
	mux.Handle("/agents/model-selection", gated(h.handleSelection))
	mux.Handle("/agents/apply", methodPOST(gated(h.handleApply)))
	mux.Handle("/agents/restore", methodPOST(gated(h.handleRestore)))
	// Deliberately public: remote machines need the configurator most.
	mux.Handle("/agents/cli", methodGET(http.HandlerFunc(h.handleCLI)))
}

func (h *AgentHandlers) local(r *http.Request) bool {
	host := requestRemoteHost(r)
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (h *AgentHandlers) handleAvailable(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, http.StatusOK, map[string]any{"enabled": h.local(r)})
}

func (h *AgentHandlers) handleOverview(w http.ResponseWriter, r *http.Request) {
	if !h.local(r) {
		writeJSONResponse(w, http.StatusOK, map[string]any{
			"enabled": false,
			"reason":  "agent config is only available from the machine running the gateway",
		})
		return
	}
	overview := h.Service.Overview(r.Context())
	clients := make([]any, 0, len(overview))
	for _, id := range agentClientIDs() {
		if entry, ok := overview[id]; ok {
			clients = append(clients, entry)
		}
	}
	selection, configured := h.Service.Selection()
	if selection == nil {
		selection = []string{}
	}
	keys := []any{}
	if h.Server != nil && h.Server.keyStore != nil {
		if entries, err := h.Server.keyStore.List(false); err == nil {
			for _, entry := range entries {
				if entry.Enabled {
					keys = append(keys, map[string]any{"id": entry.ID, "name": entry.Name, "enabled": true})
				}
			}
		}
	}
	authRequired := false
	globalKeySet := false
	if h.Server != nil {
		globalKeySet = h.Server.globalKeySet()
		authRequired = h.Server.authRequired()
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"enabled": true, "clients": clients,
		"models":          agents.GatewayModels(r.Context(), h.Service.Providers),
		"selected_models": selection, "models_configured": configured,
		"model_embedding_clients": agents.ModelEmbeddingClients(),
		"keys":                    keys, "global_key_set": globalKeySet,
		"auth_required": authRequired,
		"gateway":       map[string]any{"base_url": h.baseURLHint()},
	})
}

func (h *AgentHandlers) handleSelection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		selection, configured := h.Service.Selection()
		if selection == nil {
			selection = []string{}
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"configured": configured, "selected_models": selection})
	case http.MethodPost:
		var payload map[string]any
		if failure := decodeAgentsBody(w, r, &payload); failure != nil {
			writeFailure(w, failure)
			return
		}
		raw, present := payload["models"]
		if !present {
			writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "models is required")
			return
		}
		models, ok := agentStringList(raw)
		if !ok {
			writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "models must be a list")
			return
		}
		if err := h.Service.SetSelection(models); err != nil {
			writeV1Error(w, http.StatusInternalServerError, "server_error", "agents model selection failed")
			return
		}
		selection, _ := h.Service.Selection()
		if selection == nil {
			selection = []string{}
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "configured": true, "selected_models": selection})
	default:
		writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (h *AgentHandlers) handleApply(w http.ResponseWriter, r *http.Request) {
	if !h.local(r) {
		writeV1Error(w, http.StatusForbidden, "invalid_request_error",
			"agent config is only available from the machine running the gateway")
		return
	}
	var payload map[string]any
	if failure := decodeAgentsBody(w, r, &payload); failure != nil {
		writeFailure(w, failure)
		return
	}
	clientID := firstAgentString(payload, "client", "client_id")
	if clientID == "" {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "client is required")
		return
	}
	baseURL := firstAgentString(payload, "base_url")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error",
			"base_url must start with http:// or https://")
		return
	}
	model := firstAgentString(payload, "model")
	var models []agents.ModelSpec
	if raw, present := payload["models"]; present && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "models must be a list")
			return
		}
		models = []agents.ModelSpec{}
		for _, item := range list {
			switch typed := item.(type) {
			case string:
				if strings.TrimSpace(typed) != "" {
					models = append(models, agents.ModelSpec{ID: strings.TrimSpace(typed)})
				}
			case map[string]any:
				id := strings.TrimSpace(firstAgentString(typed, "id"))
				if id == "" {
					continue
				}
				spec := agents.ModelSpec{
					ID: id, Name: firstAgentString(typed, "name"),
					ContextWindow:  int64(agentInt(typed["context_window"])),
					MaxOutput:      int64(agentInt(typed["max_output"])),
					SupportsVision: typed["supports_vision"] == true,
				}
				if values, ok := agentStringList(typed["reasoning_efforts"]); ok {
					spec.ReasoningEfforts = values
				}
				models = append(models, spec)
			}
		}
		if len(models) > agents.MaxModels {
			models = models[:agents.MaxModels]
		}
	}
	apiKey, hardFailure := h.resolveAgentKey(payload)
	if hardFailure != "" {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", hardFailure)
		return
	}
	warnings := []string{}
	if apiKey == "" {
		apiKey = "wb-local"
		warnings = append(warnings, "gateway has no key configured; wrote placeholder 'wb-local' (auth is off, so any value works)")
	}
	result, err := h.Service.Apply(r.Context(), clientID, baseURL, apiKey, model, models)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "failed to write") {
			status = http.StatusInternalServerError
		}
		writeV1Error(w, status, "invalid_request_error", err.Error())
		return
	}
	result["warnings"] = warnings
	writeJSONResponse(w, http.StatusOK, result)
}

func (h *AgentHandlers) handleRestore(w http.ResponseWriter, r *http.Request) {
	if !h.local(r) {
		writeV1Error(w, http.StatusForbidden, "invalid_request_error",
			"agent config is only available from the machine running the gateway")
		return
	}
	var payload map[string]any
	if failure := decodeAgentsBody(w, r, &payload); failure != nil {
		writeFailure(w, failure)
		return
	}
	clientID := firstAgentString(payload, "client", "client_id")
	if clientID == "" {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "client is required")
		return
	}
	result, err := h.Service.Restore(r.Context(), clientID)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (h *AgentHandlers) handleCLI(w http.ResponseWriter, r *http.Request) {
	body := agents.StandaloneConfigurator
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="wb_agents.py"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *AgentHandlers) resolveAgentKey(payload map[string]any) (string, string) {
	keyID := firstAgentString(payload, "key_id")
	var entries []keysEntry
	if h.Server != nil && h.Server.keyStore != nil {
		list, err := h.Server.keyStore.List(false)
		if err == nil {
			for _, entry := range list {
				entries = append(entries, keysEntry{ID: entry.ID, Key: entry.Key, Enabled: entry.Enabled})
			}
		}
	}
	if keyID == "__global" {
		if h.Server != nil && strings.TrimSpace(h.Server.opts.StaticAPIKey) != "" {
			return h.Server.opts.StaticAPIKey, ""
		}
		for _, entry := range entries {
			if entry.Enabled && entry.Key != "" {
				return entry.Key, ""
			}
		}
		return "", ""
	}
	if keyID != "" {
		for _, entry := range entries {
			if entry.ID == keyID {
				if strings.TrimSpace(entry.Key) == "" {
					return "", "no configured key with id " + strconv.Quote(keyID)
				}
				return entry.Key, ""
			}
		}
		return "", "no configured key with id " + strconv.Quote(keyID)
	}
	if h.Server != nil && strings.TrimSpace(h.Server.opts.StaticAPIKey) != "" {
		return h.Server.opts.StaticAPIKey, ""
	}
	for _, entry := range entries {
		if entry.Enabled && entry.Key != "" {
			return entry.Key, ""
		}
	}
	if h.Server != nil && h.Server.keyStore != nil && h.Server.keyStore.AuthRequired() {
		return "", "no gateway key available - 请先配置 API Key"
	}
	return "", ""
}

type keysEntry struct {
	ID      string
	Key     string
	Enabled bool
}

func (h *AgentHandlers) baseURLHint() string {
	host := "127.0.0.1"
	port := 0
	if h.Server != nil {
		port = h.Server.opts.ListenerPort
		addr := strings.TrimSpace(h.Server.opts.Addr)
		if addr != "" {
			if parsedHost, parsedPort, err := net.SplitHostPort(addr); err == nil {
				if parsedHost != "" && parsedHost != "0.0.0.0" && parsedHost != "::" {
					host = parsedHost
				}
				if port == 0 {
					port, _ = strconv.Atoi(parsedPort)
				}
			}
		}
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/v1"
}

func requestRemoteHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

func decodeAgentsBody(w http.ResponseWriter, r *http.Request, target *map[string]any) *apiFailure {
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, agentsMaxBodyBytes))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return localFailure(http.StatusBadRequest, "invalid JSON body")
	}
	if *target == nil {
		return localFailure(http.StatusBadRequest, "expected a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != nil && !errors.Is(err, io.EOF) {
		return localFailure(http.StatusBadRequest, "invalid JSON body")
	}
	return nil
}

func firstAgentString(payload map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := payload[name].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func agentStringList(value any) ([]string, bool) {
	list, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]string); ok {
			return append([]string(nil), typed...), true
		}
		return nil, false
	}
	out := []string{}
	for _, item := range list {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, strings.TrimSpace(text))
		}
	}
	return out, true
}

func agentInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		number, _ := typed.Int64()
		return int(number)
	default:
		return 0
	}
}

func agentClientIDs() []string {
	out := []string{}
	for _, client := range agents.Clients() {
		out = append(out, client.ID)
	}
	sortStrings(out)
	return out
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
