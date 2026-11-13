package workbuddy

import (
	"fmt"
	"net/http"
	"strings"
)

func (p *Provider) campaignTargets(uid string) (*Provider, []*Account, string) {
	provider := p.providerForRealm(realmCN)
	if provider == nil {
		return nil, nil, "账号池不可用"
	}
	provider.refreshSettings()
	if uid != "" && uid != "all" {
		account := provider.Pool().Get(uid)
		if account == nil || accountRealmOf(account) != realmCN {
			return provider, nil, "未找到指定的国内版账号"
		}
		return provider, []*Account{account}, ""
	}
	accounts := provider.Pool().TaskAccounts()
	targets := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		if accountRealmOf(account) == realmCN {
			targets = append(targets, account)
		}
	}
	return provider, targets, ""
}

func (p *Provider) handleActivityCampaigns(w http.ResponseWriter, r *http.Request) {
	provider := p.providerForRealm(realmCN)
	if provider == nil {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"ok": false, "activities": []CampaignActivity{}, "accounts": []any{},
			"msg": "未找到参与任务的国内版账号",
		})
		return
	}
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	provider, targets, message := provider.campaignTargets(uid)
	if len(targets) == 0 {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"ok": false, "activities": []CampaignActivity{}, "accounts": []any{},
			"msg": firstNonEmpty(message, "未找到参与任务的国内版账号"),
		})
		return
	}
	selected := targets[0]
	snapshot, err := provider.CampaignSnapshot(r.Context(), selected.UID())
	if err != nil {
		writeWBAdminError(w, wbAdminErrorFrom(err))
		return
	}
	accounts := make([]map[string]any, 0, len(targets))
	for _, account := range targets {
		accounts = append(accounts, map[string]any{
			"uid":      account.UID(),
			"nickname": firstNonEmpty(accountNicknameOf(account), shortUID(account.UID())),
		})
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok":         snapshot.OK,
		"activities": snapshot.Activities,
		"account":    selected.View(),
		"accounts":   accounts,
		"logs":       snapshot.Logs,
		"msg":        snapshot.Error,
	})
}

func (p *Provider) handleActivityCampaignsRun(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	provider := p.providerForRealm(realmCN)
	if provider == nil {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "账号池不可用"})
		return
	}
	uid := wbAdminText(payload["uid"])
	provider, targets, message := provider.campaignTargets(uid)
	if len(targets) == 0 {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{
			"ok": false, "msg": firstNonEmpty(message, "未找到参与任务的国内版账号"),
		})
		return
	}
	autoClaim := true
	if raw, present := payload["claim"]; present {
		if value, ok := wbAdminBool(raw); ok {
			autoClaim = value
		}
	}
	results := make([]CampaignResult, 0, len(targets))
	for index, account := range targets {
		if index > 0 {
			provider.taskSleepFor(provider.taskGapDelay())
		}
		result, err := provider.CampaignTasks(r.Context(), account.UID(), autoClaim)
		if err != nil {
			writeWBAdminError(w, wbAdminErrorFrom(err))
			return
		}
		results = append(results, result)
	}
	logs := make([]string, 0)
	total := 0.0
	for index, result := range results {
		nick := firstNonEmpty(result.Nickname, shortUID(result.UID))
		logs = append(logs, fmt.Sprintf("====== 正在为账号 [%s (%s)] 执行运营活动任务 (%d/%d) ======", nick, result.UID, index+1, len(results)))
		for _, line := range result.Logs {
			logs = append(logs, "  "+line)
		}
		total += result.EarnedCredit
	}
	logs = append(logs, fmt.Sprintf("====== 全部 %d 个账号运营活动执行完毕，累计新增积分: +%g ======", len(results), total))
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"credit_added":   total,
		"logs":           logs,
		"accounts_count": len(results),
		"results":        results,
	})
}

func (p *Provider) handleActivityCampaignClaim(w http.ResponseWriter, r *http.Request) {
	payload, failure := readWBAdminPayload(w, r)
	if failure != nil {
		writeWBAdminError(w, failure)
		return
	}
	provider := p.providerForRealm(realmCN)
	if provider == nil {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "账号池不可用"})
		return
	}
	uid := wbAdminText(payload["uid"])
	activityCode := wbAdminText(payload["activity_code"])
	task := wbAdminText(payload["task"])
	if uid == "" || uid == "all" {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "请先选择一个国内版账号"})
		return
	}
	if activityCode == "" || task == "" {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "缺少活动代码或任务代码"})
		return
	}
	provider.refreshSettings()
	account := provider.Pool().Get(uid)
	if account == nil || accountRealmOf(account) != realmCN {
		writeWBAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "未找到指定的国内版账号"})
		return
	}
	spec, ok := campaignSpecForCode(activityCode)
	if !ok {
		writeWBAdminError(w, wbBadRequest("unknown activity_code"))
		return
	}
	if _, ok := campaignTaskSpecFor(spec, task); !ok {
		writeWBAdminError(w, wbBadRequest("unknown campaign task"))
		return
	}
	result, err := provider.CampaignClaim(r.Context(), account.UID(), activityCode, task)
	if err != nil {
		writeWBAdminError(w, wbAdminErrorFrom(err))
		return
	}
	writeWBAdminJSON(w, http.StatusOK, map[string]any{
		"ok":     result.OK,
		"msg":    result.Msg,
		"credit": result.Credit,
	})
}
