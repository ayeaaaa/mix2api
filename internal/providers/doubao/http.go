package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	botID       = "7338286299411103781"
	pcVersion   = "3.45.5"
	versionCode = "20800"
	userAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36 Edg/147.0.0.0"
)

// ErrorKind classifies an upstream exchange.
type ErrorKind int

const (
	KindBadRequest ErrorKind = iota
	KindAuth
	KindRateLimited
	KindUpstream
	KindTransient
	KindUnavailable
)

// APIError is a safe provider error. Message must never contain cookies.
type APIError struct {
	Kind        ErrorKind
	Status      int
	Message     string
	Code        any
	VerifyScene string
}

func (e *APIError) Error() string {
	if e == nil {
		return "doubao: upstream error"
	}
	if e.Status > 0 {
		return fmt.Sprintf("doubao: HTTP %d: %s", e.Status, e.Message)
	}
	return "doubao: " + e.Message
}

func (e *APIError) HTTPStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	switch e.Kind {
	case KindBadRequest:
		return http.StatusBadRequest
	case KindAuth, KindUpstream:
		return http.StatusBadGateway
	case KindRateLimited:
		return http.StatusTooManyRequests
	case KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusServiceUnavailable
	}
}

func (e *APIError) ErrorType() string {
	if e != nil && e.Kind == KindRateLimited {
		return "rate_limit_error"
	}
	return "invalid_request_error"
}

func (e *APIError) SafeMessage() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.Message)
}

func (e *APIError) GatewayHint() string {
	if e != nil && e.Kind == KindRateLimited {
		return "上游触发豆包风控验证；请稍后重试或更换出口 IP。"
	}
	return ""
}

func textBlock(text string) map[string]any {
	return map[string]any{
		"block_type": 10000,
		"content": map[string]any{
			"text_block":     map[string]any{"text": text, "icon_url": "", "icon_url_dark": "", "summary": ""},
			"pc_event_block": "",
		},
		"block_id":      randomID(),
		"parent_id":     "",
		"meta_info":     []any{},
		"extra_info":    map[string]any{},
		"append_fields": []any{},
	}
}

type conversation struct {
	ConversationID      string
	SectionID           string
	LocalConversationID string
	LastMessageIndex    any
}

func buildBody(model *Model, prompt string, conv *conversation) map[string]any {
	isWork := model.AgentMode == 1
	effort := model.DefaultReasoning
	if effort == 0 {
		effort = 5
	}
	localMessageID := randomID()
	agentTaskParam := map[string]any{
		"runtime_type": 1, "folder_name": "", "device_name": "", "workspace": "", "local_app_id": "", "local_device_id": "",
	}
	generalTaskParam := map[string]any{"server_do_compare": true}
	if isWork {
		generalTaskParam = map[string]any{
			"action": 0, "thread_local_message_id": []any{localMessageID}, "project_id": "",
			"runtime_type": 1, "agent_task_param": agentTaskParam, "server_do_compare": true,
		}
	}
	continuing := conv != nil && strings.TrimSpace(conv.ConversationID) != ""
	localConvID := ""
	if conv != nil {
		localConvID = conv.LocalConversationID
	}
	if localConvID == "" {
		localConvID = "local_" + randomID()[:12]
	}
	lastIndex := any(nil)
	sectionID := ""
	if continuing && conv != nil {
		lastIndex = conv.LastMessageIndex
		sectionID = conv.SectionID
	}
	extraParams := model.ExtraParams
	if extraParams == nil {
		extraParams = map[string]any{}
	}
	body := map[string]any{
		"client_meta": map[string]any{
			"conversation_id":    condString(continuing, conv.ConversationID, ""),
			"bot_id":             botID,
			"last_section_id":    condString(continuing, sectionID, ""),
			"last_message_index": lastIndex,
			"local_permissions": []any{
				map[string]any{"permission_name": "ACCESS_COARSE_LOCATION", "status": 3},
				map[string]any{"permission_name": "ACCESS_FINE_LOCATION", "status": 3},
				map[string]any{"permission_name": "ACCESS_BACKGROUND_LOCATION", "status": 3},
			},
		},
		"messages": []any{map[string]any{
			"local_message_id": localMessageID,
			"content_block":    []any{textBlock(prompt)},
			"message_status":   0,
		}},
		"option": map[string]any{
			"send_message_scene": "", "create_time_ms": time.Now().UnixMilli(),
			"collect_id": "", "is_audio": false, "answer_with_suggest": false,
			"agent_mode": model.AgentMode, "tts_switch": false, "need_deep_think": model.ItemID,
			"click_clear_context": false, "from_suggest": false, "is_regen": false,
			"is_replace": false, "is_from_click_option": false, "is_from_click_softlink": false,
			"disable_sse_cache": false, "select_text_action": "", "is_select_text": false,
			"resend_for_regen": false, "scene_type": 0, "unique_key": randomID(),
			"start_seq": 0, "need_create_conversation": !continuing,
			"conversation_init_option": map[string]any{"project_id": "", "need_ack_conversation": true},
			"conversation_init_ext": map[string]any{
				"model_item_key": model.Key, "reasoning_effort": fmt.Sprint(effort), "mode_id": model.ModeID,
			},
			"regen_query_id": []any{}, "edit_query_id": []any{}, "regen_instruction": "",
			"no_replace_for_regen": false, "message_from": 0, "shared_app_name": "", "shared_app_id": "",
			"sse_recv_event_options":    map[string]any{"support_chunk_delta": true},
			"support_lazy_fetch_stream": true, "is_ai_playground": false, "is_old_user": true,
			"general_task_param":   generalTaskParam,
			"recovery_option":      map[string]any{"is_recovery": false, "req_create_time_sec": time.Now().Unix(), "append_sse_event_scene": 0},
			"message_storage_type": 0, "related_deleted_message_ids": map[string]any{},
			"connector_info_list": []any{},
			"model_config": map[string]any{
				"model_item_key": model.Key, "model_extra_params": extraParams, "reasoning_effort": effort,
			},
			"aggregate_params": map[string]any{
				"mention_skill_list": "[]", "mention_plugin_list": "[]", "mention_ext": "[{}]",
				"conversation_mode": fmt.Sprint(model.ConversationMode), "mode_id": model.ModeID,
				"model_item_key": model.Key, "agent_mode": fmt.Sprint(model.AgentMode),
				"reasoning_effort": fmt.Sprint(effort), "provider_id": "",
			},
			"conversation_mode": model.ConversationMode,
		},
		"user_context": []any{},
		"ext": map[string]any{
			"general_task_param":            mustJSON(generalTaskParam),
			"use_deep_think":                fmt.Sprint(model.ItemID),
			"agent_mode":                    fmt.Sprint(model.AgentMode),
			"conversation_init_option":      mustJSON(map[string]any{"project_id": "", "need_ack_conversation": true}),
			"sub_conv_firstmet_type":        "1",
			"collection_id":                 "",
			"is_finish":                     "1",
			"commerce_credit_config_enable": "0",
		},
	}
	if isWork {
		if init, ok := body["option"].(map[string]any)["conversation_init_ext"].(map[string]any); ok {
			init["agent_task_param"] = mustJSON(agentTaskParam)
		}
	}
	if !continuing {
		body["client_meta"].(map[string]any)["local_conversation_id"] = localConvID
	}
	return body
}

func condString(ok bool, left, right string) string {
	if ok {
		return left
	}
	return right
}

func mustJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func (p *Provider) buildQuery(account *Account) string {
	values := url.Values{}
	values.Set("aid", p.currentConfig().AID)
	values.Set("device_id", account.deviceValue("web_id"))
	values.Set("device_platform", "web")
	values.Set("doubao_device_platform", "web")
	values.Set("doubao_pc_version", pcVersion)
	values.Set("fp", decodeCookieValue(account.cookie("s_v_web_id")))
	values.Set("language", "zh")
	values.Set("pc_version", pcVersion)
	values.Set("pkg_type", "release_version")
	values.Set("real_aid", p.currentConfig().AID)
	values.Set("region", "CN")
	values.Set("samantha_web", "1")
	values.Set("sys_region", "CN")
	values.Set("tea_uuid", account.deviceValue("tea_uuid"))
	values.Set("tz_name", "Asia/Shanghai")
	values.Set("use-olympus-account", "1")
	values.Set("version_code", versionCode)
	values.Set("web_id", account.deviceValue("web_id"))
	values.Set("web_platform", "browser")
	webTabID := account.deviceValue("web_tab_id")
	if webTabID == "" {
		webTabID = randomID()
	}
	values.Set("web_tab_id", webTabID)
	return values.Encode()
}

func (p *Provider) openUpstream(ctx context.Context, account *Account, model *Model, prompt string, conv *conversation) (io.ReadCloser, error) {
	cfg := p.currentConfig()
	base := strings.TrimRight(cfg.BaseURL, "/")
	body := buildBody(model, prompt, conv)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completion?"+p.buildQuery(account), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Agw-Js-Conv", "str")
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", "zh-CN,zh;q=0.9")
	req.Header.Set("cookie", account.cookieHeader())
	req.Header.Set("origin", base)
	req.Header.Set("referer", base+"/chat/")
	req.Header.Set("user-agent", userAgent)

	client, err := p.clientForProxy(p.proxyURL(account))
	if err != nil {
		return nil, &APIError{Kind: KindTransient, Message: "proxy slot is misconfigured"}
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("doubao: upstream transport failed")
		return nil, &APIError{Kind: KindTransient, Message: "upstream request failed"}
	}
	contentType := resp.Header.Get("content-type")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		message := strings.TrimSpace(string(body))
		if len(message) > 400 {
			message = message[:400]
		}
		kind := KindUpstream
		if resp.StatusCode == http.StatusTooManyRequests {
			kind = KindRateLimited
		}
		if strings.Contains(strings.ToLower(message), "login") || resp.StatusCode == http.StatusUnauthorized {
			kind = KindAuth
			message = "登录已过期或 cookie 无效，请重新导出 cookie"
		}
		if message == "" {
			message = "upstream rejected the request"
		}
		return nil, &APIError{Kind: kind, Status: resp.StatusCode, Message: message}
	}
	return resp.Body, nil
}
