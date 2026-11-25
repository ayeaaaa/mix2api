package codearts

import "testing"

func TestStationProfiles(t *testing.T) {
	if Mainland.Realm != "codearts" || Mainland.SettingsKey != "codearts" || Mainland.AccountsFolder != "codearts" {
		t.Fatalf("mainland identity = %#v", Mainland)
	}
	if Mainland.SnapHost != "https://snap-access.cn-north-4.myhuaweicloud.com" ||
		Mainland.Portal != "https://codearts.huaweicloud.com/portal" ||
		Mainland.Locale != "zh-cn" ||
		!Mainland.SupportsBenefit || !Mainland.SupportsCheckin {
		t.Fatalf("mainland profile = %#v", Mainland)
	}
	if International.Realm != "codearts-intl" || International.SettingsKey != "codearts-intl" || International.AccountsFolder != "codearts-intl" {
		t.Fatalf("international identity = %#v", International)
	}
	if International.SnapHost != "https://snap-access.ap-southeast-1.myhuaweicloud.com" ||
		International.Portal != "https://codearts.ap-southeast-1.huaweicloud.com/portal" ||
		International.LoginURL != "https://auth.huaweicloud.com/authui/login?site=intl" ||
		International.Locale != "en-us" ||
		International.SupportsBenefit || International.SupportsCheckin {
		t.Fatalf("international profile = %#v", International)
	}
	if got := International.SnapManagerBase(); got != International.SnapHost+"/snap-manager" {
		t.Fatalf("SnapManagerBase = %q", got)
	}
}
