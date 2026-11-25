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

// SnapManagerBase returns the station's snap-manager root without a trailing
// slash.
func (s Station) SnapManagerBase() string {
	return strings.TrimRight(s.SnapHost, "/") + "/snap-manager"
}

func stationOrDefault(station Station) Station {
	if station.ID == "" {
		return Mainland
	}
	return station
}
