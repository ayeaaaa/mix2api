package dashboard

import (
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/web"
)

func TestDashboardUsesOneLayout(t *testing.T) {
	html := string(web.DashboardHTML())
	for _, marker := range []string{
		`document.documentElement.setAttribute('data-ui', 'next')`,
		`window.__NEXT_UI__ = true`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("single layout marker missing: %q", marker)
		}
	}
	for _, forbidden := range []string{
		`id="nextLayoutOption"`,
		`id="consoleLayoutOption"`,
		`localStorage.getItem('wb-ui-mode')`,
		`localStorage.getItem('wb-console-layout')`,
		`function toggleNextLayout`,
		`function toggleConsoleLayout`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("layout switch still present: %q", forbidden)
		}
	}
}

func TestDashboardNavigationIsGroupedAndModuleSpecific(t *testing.T) {
	html := string(web.DashboardHTML())
	if count := strings.Count(html, "next-nav-caption"); count < 3 {
		t.Fatalf("next nav captions = %d, want at least 3", count)
	}
	for _, label := range []string{
		"总览",
		"账号与提供商",
		"模型",
		"用量",
		"自动化",
		"媒体",
		"智能体",
		"设置",
		"日志",
	} {
		if !strings.Contains(html, ">"+label+"<") {
			t.Fatalf("navigation label missing: %q", label)
		}
	}
	if strings.Contains(html, ">网关<") {
		t.Fatal("ambiguous gateway navigation label still present")
	}
}

func TestDashboardSettingsHaveProviderAndRoutingGroups(t *testing.T) {
	html := string(web.DashboardHTML())
	for _, group := range []string{
		`data-settings-group="basic"`,
		`data-settings-group="providers"`,
		`data-settings-group="models"`,
		`data-settings-group="routing"`,
		`data-settings-group="automation"`,
		`data-settings-group="stability"`,
		`data-settings-group="system"`,
	} {
		if !strings.Contains(html, group) {
			t.Fatalf("settings group missing: %q", group)
		}
	}
	if strings.Contains(html, `data-settings-group="api"`) {
		t.Fatal("old mixed API settings group still present")
	}
	if !strings.Contains(html, "next-settings-groups") || !strings.Contains(html, "position:sticky") {
		t.Fatal("settings group rail is not present")
	}
	if !strings.Contains(html, "analytics: { title: '数据看板'") {
		t.Fatal("usage page is not using the data-dashboard title")
	}
	if !strings.Contains(html, "#pageAnalytics.page-nav-on.active") {
		t.Fatal("usage page does not restore the console data-dashboard rail")
	}
}

func TestDashboardMobileSidebarBackdropIsClickable(t *testing.T) {
	html := string(web.DashboardHTML())
	if !strings.Contains(html, `html[data-ui="next"] #nextSidebarBackdrop`) {
		t.Fatal("mobile sidebar backdrop selector does not override the base display:none rule")
	}
	if !strings.Contains(html, `onclick="nextOpenSidebar(false)"`) {
		t.Fatal("mobile sidebar backdrop does not close the sidebar")
	}
}

func TestDashboardProviderTabsAreExplicit(t *testing.T) {
	html := string(web.DashboardHTML())
	for _, label := range []string{
		"WorkBuddy 国际",
		"WorkBuddy 国内",
		"CodeArts",
		"小浣熊",
	} {
		if !strings.Contains(html, label) {
			t.Fatalf("provider tab label missing: %q", label)
		}
	}
	if !strings.Contains(html, "accountsRealmSwitch") {
		t.Fatal("provider tab container missing")
	}
}
