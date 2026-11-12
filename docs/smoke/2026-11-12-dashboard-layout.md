# Dashboard single-layout verification — 2026-11-12

Unreleased UI cleanup on `v2` after the `v2.2.0` release.

## Browser checks

Chrome headless (CDP), viewport `1440x1000` and `800x1000`, authenticated panel:

- `document.documentElement.dataset.ui === "next"`; no classic/console switch
  entries remain in the theme menu.
- Sidebar captions: `工作台 / 资源 / 系统`.
- Sidebar entries: `总览 / 用量 / 账号与提供商 / 模型 / 自动化 / 媒体 / 智能体 /
  设置 / 日志`.
- Accounts page provider tabs: `WorkBuddy 国际`, `WorkBuddy 国内`, `CodeArts`,
  `小浣熊`; only the selected provider's account sections are visible.
- Settings page at 1440: left rail `216px`, content `906px`; the seven group
  buttons are visible and only the selected group's sections render.
- Settings groups and visible section counts:
  - 基础: 6
  - 提供商: 4
  - 模型与定价: 6
  - 路由与限额: 5
  - 自动化: 3
  - 稳定性: 8
  - 系统: 5
- At 800px the settings grid collapses to one column and the group rail becomes
  a horizontal scroller; no horizontal overflow.
- No JavaScript exceptions were observed. Only Chrome's verbose advisory that
  password inputs are not inside a form was logged.

## Automated checks

```sh
GOTOOLCHAIN=go1.26.11 make check
GOTOOLCHAIN=go1.26.11 make race
```

Both pass, including the dashboard static contract, Node UI harness, and the new
single-layout/navigation/settings-group regression tests.

## Notes

- Legacy `/classic` URLs still return the same dashboard bytes for compatibility;
  they no longer select a different layout.
- The existing settings data fields are unchanged; only their grouping and
  presentation changed.
