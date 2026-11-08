package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// CheckinCampaignResult is one v1 campaign row from a check-in run. CampaignID
// preserves the upstream scalar so an ID sent as a JSON number is echoed as a
// number. Credits is present only after a claim/confirm branch, matching v1.
type CheckinCampaignResult struct {
	CampaignID any      `json:"campaign_id"`
	Title      string   `json:"title"`
	OK         bool     `json:"ok"`
	Status     string   `json:"status"`
	Credits    *float64 `json:"credits,omitempty"`
	Message    string   `json:"message"`
}

// CheckinResult is both the single-account response and, with Results filled,
// the all-accounts response. Campaigns is used for a single account; Results is
// used by CheckinAll.
type CheckinResult struct {
	OK        bool                    `json:"ok"`
	Credits   float64                 `json:"credits"`
	Message   string                  `json:"message"`
	Campaigns []CheckinCampaignResult `json:"campaigns,omitempty"`
	Results   []CheckinAccountResult  `json:"results,omitempty"`

	batch bool
}

// CheckinAccountResult is one per-account row in CheckinAll.
type CheckinAccountResult struct {
	UID       string                  `json:"uid"`
	OK        bool                    `json:"ok"`
	Credits   float64                 `json:"credits"`
	Message   string                  `json:"message"`
	Campaigns []CheckinCampaignResult `json:"campaigns,omitempty"`
}

// MarshalJSON keeps v1's two response shapes: a single account always has a
// campaigns array (even when empty), while the all-accounts envelope always has
// a results array and no top-level campaigns.
func (r CheckinResult) MarshalJSON() ([]byte, error) {
	if r.batch || r.Results != nil {
		results := r.Results
		if results == nil {
			results = make([]CheckinAccountResult, 0)
		}
		return json.Marshal(struct {
			OK      bool                   `json:"ok"`
			Credits float64                `json:"credits"`
			Message string                 `json:"message"`
			Results []CheckinAccountResult `json:"results"`
		}{OK: r.OK, Credits: r.Credits, Message: r.Message, Results: results})
	}
	campaigns := r.Campaigns
	if campaigns == nil {
		campaigns = make([]CheckinCampaignResult, 0)
	}
	return json.Marshal(struct {
		OK        bool                    `json:"ok"`
		Credits   float64                 `json:"credits"`
		Message   string                  `json:"message"`
		Campaigns []CheckinCampaignResult `json:"campaigns"`
	}{OK: r.OK, Credits: r.Credits, Message: r.Message, Campaigns: campaigns})
}

// MarshalJSON keeps CheckinAccountResult's UID alongside the v1 account fields.
func (r CheckinAccountResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		UID       string                  `json:"uid"`
		OK        bool                    `json:"ok"`
		Credits   float64                 `json:"credits"`
		Message   string                  `json:"message"`
		Campaigns []CheckinCampaignResult `json:"campaigns,omitempty"`
	}{UID: r.UID, OK: r.OK, Credits: r.Credits, Message: r.Message, Campaigns: r.Campaigns})
}

// isCheckinCampaign mirrors v1's type/event classifier. Historical campaign
// IDs 1 and 4 remain accepted, but the daily login campaign is selected by type
// or trigger event so seasonal ID changes cannot silently skip it.
func isCheckinCampaign(item map[string]any) bool {
	if item == nil {
		return false
	}
	if campaignIDIsCheckin(item["campaignId"]) {
		return true
	}
	if strings.ToUpper(modelTruthyText(item["type"])) == "USER_LOGIN" {
		return true
	}
	extra := modelMap(item["extra"])
	if extra == nil {
		return false
	}
	return strings.ToLower(modelTruthyText(extra["triggerEvent"])) == "user.login"
}

// campaignIDIsCheckin matches Python's membership test against (1, 4): JSON
// numbers and Go numeric values compare by value, bool True equals 1, but a
// string "1"/"4" is not a historical check-in ID.
func campaignIDIsCheckin(value any) bool {
	var number float64
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return false
		}
		number = parsed
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case int8:
		number = float64(typed)
	case int16:
		number = float64(typed)
	case int32:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case uint:
		number = float64(typed)
	case uint8:
		number = float64(typed)
	case uint16:
		number = float64(typed)
	case uint32:
		number = float64(typed)
	case uint64:
		number = float64(typed)
	case bool:
		if !typed {
			return false
		}
		number = 1
	default:
		return false
	}
	return number == 1 || number == 4
}

// Checkin runs one manual account check-in. Manual callers may target a
// disabled account, matching v1's panel action; a concurrent call for the same
// account gets v1's "正在签到" result instead of double-claiming.
func (p *Provider) Checkin(ctx context.Context, uid string) (CheckinResult, error) {
	if p == nil {
		return CheckinResult{}, errors.New("codearts: provider is nil")
	}
	if p.pool == nil {
		return CheckinResult{}, errors.New("codearts: account pool is nil")
	}
	if ctx == nil {
		return CheckinResult{}, errors.New("codearts: check-in requires a context")
	}
	account := p.pool.Get(strings.TrimSpace(uid))
	if account == nil {
		return CheckinResult{}, ErrAccountNotFound
	}
	return p.runCheckin(ctx, account)
}

// CheckinAll checks in every live, enabled account. Disabled/manual-disabled
// accounts are skipped, matching v1; per-account errors are redacted rows and
// never abort the remaining accounts. A canceled context is returned to the
// caller with the rows completed so far.
func (p *Provider) CheckinAll(ctx context.Context) (CheckinResult, error) {
	if p == nil {
		return CheckinResult{}, errors.New("codearts: provider is nil")
	}
	if p.pool == nil {
		return CheckinResult{}, errors.New("codearts: account pool is nil")
	}
	if ctx == nil {
		return CheckinResult{}, errors.New("codearts: check-in requires a context")
	}

	result := CheckinResult{
		OK:      true,
		Results: make([]CheckinAccountResult, 0),
		batch:   true,
	}
	total := float64(0)
	for _, account := range p.pool.Accounts() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		uid, live := account.currentUID()
		if !live {
			continue
		}
		_, disabled := creditsAccountLabels(account)
		if disabled {
			continue
		}

		row, err := p.runCheckin(ctx, account)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			row = CheckinResult{
				OK:      false,
				Message: redactedCreditsError(err, creditsAccountAuth(account), creditErrorMessageRunes),
			}
		}
		result.Results = append(result.Results, CheckinAccountResult{
			UID:       uid,
			OK:        row.OK,
			Credits:   row.Credits,
			Message:   row.Message,
			Campaigns: row.Campaigns,
		})
		total += row.Credits
		if !row.OK {
			result.OK = false
		}
	}
	result.Credits = total
	result.Message = fmt.Sprintf("已处理 %d 个账号，共领取 %g 积分", len(result.Results), total)
	return result, nil
}

// runCheckin serializes one account's check-in. The per-account lock is
// intentionally non-blocking: a second panel click or the automatic path must
// observe "already running" rather than issuing a second claim.
func (p *Provider) runCheckin(ctx context.Context, account *Account) (CheckinResult, error) {
	if account == nil {
		return CheckinResult{}, ErrAccountNotFound
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return CheckinResult{}, ctxErr
	}
	if !account.checkinMu.TryLock() {
		return CheckinResult{
			OK:        false,
			Credits:   0,
			Campaigns: make([]CheckinCampaignResult, 0),
			Message:   creditCheckinBusyMessage,
		}, nil
	}
	defer account.checkinMu.Unlock()

	if p.tokenManager != nil {
		if err := p.tokenManager.Ensure(ctx, account); err != nil {
			return CheckinResult{}, err
		}
	}

	signed := checkinSignedHeaders()
	data, err := p.signedJSON(ctx, account, signedJSONRequest{
		Method:  http.MethodGet,
		URL:     snapHost() + creditEndpointDelivery + "?channel=" + creditCheckinChannel,
		Timeout: checkinTimeout,
		Signed:  signed,
	})
	if err != nil {
		return CheckinResult{}, err
	}

	items := modelList(modelMap(data["data"])["items"])
	results := make([]CheckinCampaignResult, 0, len(items))
	totalCredits := float64(0)
	auth := creditsAccountAuth(account)
	for _, raw := range items {
		item := modelMap(raw)
		if item == nil || !isCheckinCampaign(item) {
			continue
		}
		campaignID := item["campaignId"]
		unit := item["benefitUnit"]
		if modelTruthy(unit) && modelText(unit) != creditRequiredUnit {
			continue
		}

		title := modelTruthyText(item["title"])
		if title == "" {
			title = fmt.Sprintf(creditCampaignFallbackFmt, formatCampaignID(campaignID))
		}
		title = redactCheckinText(title, auth)
		status := strings.TrimSpace(modelTruthyText(item["status"]))
		campaign := CheckinCampaignResult{
			CampaignID: campaignID,
			Title:      title,
			Status:     status,
		}

		switch status {
		case creditConfirmedStatus, creditConsumedStatus:
			campaign.OK = true
			campaign.Message = "已领取"
			results = append(results, campaign)
			continue
		case creditEligibleStatus:
			if !modelTruthy(item["claimable"]) {
				campaign.OK = false
				campaign.Message = "暂不可领取，稍后重试"
				results = append(results, campaign)
				continue
			}
			if err := p.claimCampaign(ctx, account, campaignID); err != nil {
				return CheckinResult{}, err
			}
			totalCredits += campaignCreditAmount(item)
			status = creditClaimedStatus
		}

		if status == creditClaimedStatus || status == creditEligibleStatus {
			if err := p.confirmCampaign(ctx, account, campaignID); err != nil {
				return CheckinResult{}, err
			}
			credited := totalCredits
			campaign.OK = true
			campaign.Status = creditSuccessStatus
			campaign.Credits = &credited
			if totalCredits != 0 {
				campaign.Message = fmt.Sprintf("领取成功（%g 积分）", totalCredits)
			} else {
				campaign.Message = "已补确认"
			}
			results = append(results, campaign)
			continue
		}

		campaign.OK = false
		campaign.Message = "未获得活动资格"
		results = append(results, campaign)
	}

	messages := make([]string, 0, len(results))
	for _, campaign := range results {
		messages = append(messages, campaign.Title+": "+campaign.Message)
	}
	message := strings.Join(messages, "; ")
	if message == "" {
		message = creditNoCampaignsMessage
	}
	ok := true
	for _, campaign := range results {
		if !campaign.OK {
			ok = false
			break
		}
	}
	result := CheckinResult{
		OK:        ok,
		Credits:   totalCredits,
		Message:   message,
		Campaigns: results,
	}

	now := p.catalogNow()
	account.setCheckin(result.OK, result.Message, now)
	if totalCredits > 0 {
		// A zero-credit snapshot must not keep the account parked after the
		// check-in actually added credits; the next overview refetches it.
		account.ClearCreditExhausted()
	}
	// Always expire the info cache timestamp, even when no credits arrived, so
	// a previously failed/partial snapshot is refreshed by the next overview.
	account.invalidateInfo()
	if account.pool != nil {
		if err := account.pool.SaveState(); err != nil {
			return result, err
		}
	}
	return result, nil
}

func checkinSignedHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Language", creditCheckinLanguage)
	headers.Set("Agent-Type", creditCheckinAgentType)
	return headers
}

func (p *Provider) claimCampaign(ctx context.Context, account *Account, campaignID any) error {
	payload := map[string]any{
		"campaignId":    campaignID,
		"channel":       creditCheckinChannel,
		"idempotentKey": "claim_" + formatCampaignID(campaignID) + "_" + fmt.Sprint(p.catalogNow().UnixMilli()),
	}
	_, err := p.signedJSON(ctx, account, signedJSONRequest{
		Method:  http.MethodPost,
		URL:     snapHost() + creditEndpointClaim,
		Payload: payload,
		Timeout: checkinTimeout,
		Signed:  checkinSignedHeaders(),
	})
	return err
}

func (p *Provider) confirmCampaign(ctx context.Context, account *Account, campaignID any) error {
	_, err := p.signedJSON(ctx, account, signedJSONRequest{
		Method:  http.MethodPost,
		URL:     snapHost() + creditEndpointConfirm,
		Payload: map[string]any{"campaignId": campaignID},
		Timeout: checkinTimeout,
		Signed:  checkinSignedHeaders(),
	})
	return err
}

// campaignCreditAmount mirrors v1's `pendingTotalAmount or benefitAmount or 0`
// first-truthy selection.
func campaignCreditAmount(item map[string]any) float64 {
	if value := item["pendingTotalAmount"]; modelTruthy(value) {
		return creditNumber(value)
	}
	if value := item["benefitAmount"]; modelTruthy(value) {
		return creditNumber(value)
	}
	return 0
}

// formatCampaignID mirrors Python's str() rendering used in idempotent keys and
// fallback titles.
func formatCampaignID(value any) string {
	if value == nil {
		return "None"
	}
	text := modelText(value)
	if text == "" {
		return fmt.Sprint(value)
	}
	return text
}

// redactCheckinText removes known credentials and proxy userinfo from upstream
// campaign titles before they are returned or persisted in state.json.
func redactCheckinText(text string, auth Auth) string {
	values := codeartsSecretValues(auth)
	if strings.TrimSpace(auth.AccessKeyID) != "" {
		values = append(values, auth.AccessKeyID)
	}
	return maskProxyURL(redactText(text, values))
}
