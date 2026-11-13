package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const (
	taskDesktopReportDomain       = "copilot.tencent.com"
	taskDesktopReportCommit       = "5f9692923c93033111c51ad7b003eb80204a9b75"
	taskDesktopReportDate   int64 = 1789036585355
)

// taskDesktopUserAgent mirrors v1's DESKTOP_UA_CN shape without hard-coding a
// version that would drift from the provider's configured desktop identity.
func (p *Provider) taskDesktopUserAgent() string {
	version := ""
	if p != nil {
		version = strings.TrimSpace(p.DesktopVersion())
	}
	if version == "" {
		version = defaultDesktopVersionCN
	}
	return "WorkBuddy/" + version + " WorkBuddy/" + version + " CLI/2.161.0"
}

func desktopReportEnvelope(auth Auth, now time.Time, version string) map[string]any {
	uid := strings.TrimSpace(auth.UID)
	nickname := strings.TrimSpace(auth.Nickname)
	if version == "" {
		version = defaultDesktopVersionCN
	}
	return map[string]any{
		"timezone": "Asia/Shanghai", "reportDelay": 2000,
		"userId": uid, "username": nickname, "userNickname": nickname,
		"product": "SaaS", "releaseDate": taskDesktopReportDate,
		"commit": taskDesktopReportCommit, "ideName": "WorkBuddy",
		"ideType": "WorkBuddy", "ideVersion": version,
		"machineId": creditDerivedID(uid, "machine"),
		"sessionId": creditDerivedID(uid, "session"),
		"extName":   "workbuddy-desktop", "extVersion": version,
		"os": "win32", "arch": "x64", "osVersion": "10.0.26220",
		"cpuCores": 20, "memorySize": 24,
		"timestamp": now.UnixMilli(), "presentAt": now.UnixMilli(),
	}
}

func (p *Provider) desktopReportHeaders(account *Account) http.Header {
	auth := taskAuthSnapshot(account)
	headers := p.taskChatHeaders(account, false)
	headers.Set("Content-Type", "application/json;charset=UTF-8")
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("User-Agent", p.taskDesktopUserAgent())
	headers.Set("X-Domain", taskDesktopReportDomain)
	headers.Set("X-Product", "SaaS")
	headers.Set("X-Request-ID", randomHex(16))
	if uid := strings.TrimSpace(auth.UID); uid != "" {
		headers.Set("X-User-Id", uid)
	}
	return headers
}

// reportDesktopEvents posts desktop-domain events to the chat report endpoint.
// The desktop endpoint commonly returns an empty body, so a 2xx response is the
// transport contract; parsing JSON here would turn success into a retryable
// failure. That is why this path uses taskDoRaw instead of taskDo.
func (p *Provider) reportDesktopEvents(ctx context.Context, account *Account, events []map[string]any) bool {
	if len(events) == 0 {
		return true
	}
	auth := taskAuthSnapshot(account)
	now := p.taskNowTime()
	envelope := desktopReportEnvelope(auth, now, p.DesktopVersion())
	payload := make([]map[string]any, 0, len(events))
	for _, event := range events {
		item := cloneAnyMap(envelope)
		for key, value := range event {
			item[key] = value
		}
		payload = append(payload, item)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	status, _, err := p.taskDoRaw(ctx, account, http.MethodPost,
		p.taskChatBase()+tasksReportPath, p.desktopReportHeaders(account),
		body, tasksDefaultTimeout)
	if err != nil {
		return false
	}
	return status >= 200 && status < 300
}
