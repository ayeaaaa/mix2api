package codearts

import "strings"

// Station describes one CodeArts deployment. The mainland and international
// sites share the same upstream protocol but differ in hosts, portal, locale,
// quota and the availability of benefit/check-in features.
type Station struct {
	ID              string // "mainland" / "international"
	ProviderID      string // "codearts" / "codearts-intl"
	Realm           string
	SettingsKey     string
	AccountsFolder  string
	SnapHost        string
	STS             string
	BenefitHost     string // empty for international
	Portal          string
	LoginURL        string // empty for mainland; intl auth page
	Locale          string
	SupportsBenefit bool
	SupportsCheckin bool
	QuotaUnit       string // "credit" / "token"
	RoutePrefix     string // "/codearts" / "/codearts-intl"
	ModelSeeds      []string
}

// Mainland is the existing cn-north-4 CodeArts deployment.
var Mainland = Station{
	ID:              "mainland",
	ProviderID:      ProviderID,
	Realm:           ProviderRealm,
	SettingsKey:     "codearts",
	AccountsFolder:  "codearts",
	SnapHost:        oauthDefaultSnapHost,
	STS:             oauthDefaultSTS,
	BenefitHost:     catalogDefaultBenefitHost,
	Portal:          oauthDefaultPortal,
	Locale:          "zh-cn",
	SupportsBenefit: true,
	SupportsCheckin: true,
	QuotaUnit:       "credit",
	RoutePrefix:     "/codearts",
	ModelSeeds:      nil,
}

// International is the ap-southeast-1 CodeArts deployment. It has no opengw
// benefit channel and no daily check-in; its Free plan is metered in tokens.
var International = Station{
	ID:              "international",
	ProviderID:      "codearts-intl",
	Realm:           "codearts-intl",
	SettingsKey:     "codearts-intl",
	AccountsFolder:  "codearts-intl",
	SnapHost:        "https://snap-access.ap-southeast-1.myhuaweicloud.com",
	STS:             oauthDefaultSTS,
	BenefitHost:     "",
	Portal:          "https://codearts.ap-southeast-1.huaweicloud.com/portal",
	LoginURL:        "https://auth.huaweicloud.com/authui/login?site=intl",
	Locale:          "en-us",
	SupportsBenefit: false,
	SupportsCheckin: false,
	QuotaUnit:       "token",
	RoutePrefix:     "/codearts-intl",
	ModelSeeds:      []string{"glm-5.3", "GLM-5.2", "Qwen3-VL-235B"},
}

// ResolvedSnapHost returns the station's snap host. Mainland keeps the legacy
// WB_CODEARTS_* env override so existing tests and private deployments work.
func (s Station) ResolvedSnapHost() string {
	if s.ID == "mainland" {
		return strings.TrimRight(snapHost(), "/")
	}
	return strings.TrimRight(s.SnapHost, "/")
}

// ResolvedPortal returns the station's portal host, preserving the mainland
// WB_CODEARTS_PORTAL_HOST override.
func (s Station) ResolvedPortal() string {
	if s.ID == "mainland" {
		return strings.TrimRight(portalHost(), "/")
	}
	return strings.TrimRight(s.Portal, "/")
}

// ResolvedSTS returns the station's STS host, preserving the mainland
// WB_CODEARTS_STS_HOST override.
func (s Station) ResolvedSTS() string {
	if s.ID == "mainland" {
		return strings.TrimRight(stsHost(), "/")
	}
	return strings.TrimRight(s.STS, "/")
}

// ResolvedBenefitHost returns the station's benefit host, preserving the
// mainland WB_CODEARTS_BENEFIT_HOST override.
func (s Station) ResolvedBenefitHost() string {
	if s.ID == "mainland" {
		return strings.TrimRight(benefitHost(), "/")
	}
	return strings.TrimRight(s.BenefitHost, "/")
}

// SnapManagerBase returns the station's snap-manager root without a trailing
// slash. Mainland keeps the WB_CODEARTS_SNAP_MANAGER override used by tests and
// private deployments.
func (s Station) SnapManagerBase() string {
	if s.ID == "mainland" {
		return snapManagerBase()
	}
	return s.ResolvedSnapHost() + "/snap-manager"
}

func stationOrDefault(station Station) Station {
	if station.ID == "" {
		return Mainland
	}
	return station
}
