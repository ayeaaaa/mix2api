# mix2api 默认图标 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the selected C (negative-space M) icon as mix2api's default favicon and sidebar mark, while preserving custom brand icon overrides.

**Architecture:** Embed the SVG and a generated ICO in `internal/web`; serve them from the exact-path static handler. The dashboard head link and sidebar default to the SVG, and `applyBranding` falls back to it whenever `site_icon` is empty.

**Tech Stack:** Go `embed`/`net/http`, embedded dashboard HTML/JS, Node dashboard suite, ImageMagick/CairoSVG only to generate the committed ICO.

**Spec:** `docs/superpowers/specs/2026-11-26-mix2api-default-icon-design.md`

## Global Constraints

- Icon is C: indigo/cyan gradient rounded square, dark negative-space M, two pale bottom endpoints.
- No external image/CDN/runtime dependency.
- Custom `site_icon` always wins.
- `/favicon.svg` and `/favicon.ico` are public, `Cache-Control: no-store`.
- Dashboard and related oracle hashes must be updated.
- Do not change provider, account, scheduler or API contracts.

## Review Focus

- A fresh install with empty `site_icon` shows the default in both tab and sidebar.
- A custom data-URI/URL icon replaces both tab and sidebar and survives `loadSettings`.
- Reset branding returns both surfaces to the default icon.
- `/favicon.ico` and `/favicon.svg` are public, return 200, and are not directory-served.
- The dashboard's existing hash/oracle/boundary tests still pin the exact asset.

---

### Task 1: Add embedded default icon assets and public routes

**Files:**
- Create: `internal/web/favicon.svg`
- Create: `internal/web/favicon.ico`
- Modify: `internal/web/assets.go`
- Modify: `internal/api/server.go:489-495`
- Test: `internal/web/assets_test.go`
- Test: `tests/dashboard/contract_test.go`
- Test: `tests/dashboard/routes_test.go`

**Interfaces:**
- Produces: `web.FaviconSVG() []byte`, `web.FaviconICO() []byte`
- Produces: public routes `/favicon.svg`, `/favicon.ico`

- [ ] **Step 1: Write the failing Go test**

Add to `internal/web/assets_test.go`:

```go
func TestDashboardServesFavicons(t *testing.T) {
	handler := Handler()
	for _, test := range []struct{ path, contentType string }{
		{"/favicon.svg", "image/svg+xml"},
		{"/favicon.ico", "image/x-icon"},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, test.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", test.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != test.contentType {
			t.Fatalf("%s content-type = %q", test.path, got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s cache-control = %q", test.path, got)
		}
	}
}
```

Remove the old `/favicon.ico` 404 expectation from `TestDashboardRejectsUnknownAndTraversalPaths`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/web -run TestDashboardServesFavicons -count=1`
Expected: FAIL because `/favicon.svg` is 404 and the embedded assets do not exist.

- [ ] **Step 3: Add the assets and routes**

Create `internal/web/favicon.svg` with the selected C design. Generate `internal/web/favicon.ico` from it at 16, 32, 48 and 64 px.

In `assets.go`, embed both files:

```go
//go:embed favicon.svg
var faviconSVG []byte

//go:embed favicon.ico
var faviconICO []byte

func FaviconSVG() []byte { return append([]byte(nil), faviconSVG...) }
func FaviconICO() []byte { return append([]byte(nil), faviconICO...) }
```

Add `serveFavicon(w, r, body, contentType)` with `Content-Type`, `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, `Content-Length` and HEAD handling. Route `/favicon.svg` and `/favicon.ico` through it in `serve`.

Add `/favicon.svg` to the dashboard aliases in `internal/api/server.go`:

```go
"/{$}", "/dashboard", "/dashboard.html", "/ui", "/classic", "/classic/{$}",
"/favicon.ico", "/favicon.svg",
```

- [ ] **Step 4: Run the Go tests to verify they pass**

Run: `go test ./internal/web ./tests/dashboard -count=1`
Expected: PASS except tests that pin the old dashboard hash, which are addressed after the dashboard edit.

- [ ] **Step 5: Commit**

```bash
git add internal/web/favicon.svg internal/web/favicon.ico \
  internal/web/assets.go internal/web/assets_test.go \
  internal/api/server.go tests/dashboard/contract_test.go tests/dashboard/routes_test.go
git commit -m "feat(web): serve default favicon assets"
```

### Task 2: Use the default icon in the dashboard and branding flow

**Files:**
- Modify: `internal/web/dashboard.html:7,2649-2650,5651,17761-17807,17780-17807`
- Create: `tests/dashboard/node/_test_site_icon.js`
- Modify: `tests/dashboard/node/run.js`
- Modify: `tests/dashboard/contract_test.go` (Node artifact list)

**Interfaces:**
- Consumes: `/favicon.svg` from Task 1.
- Produces: `DEFAULT_SITE_ICON` fallback used by `applyBranding` and `setBrandIconState`.

- [ ] **Step 1: Write the failing Node suite**

Create `_test_site_icon.js` that installs the shared DOM stub, maps `#nextSidebar .next-brand-mark` to a fake element, evaluates the dashboard script, and asserts. Because the DOM stub does not implement `textContent = ''` as child removal, use the last child after each `applyBranding` call:

```js
const dom = require('./_dom_stub.js');
const mark = dom.makeElement('div');
dom.installDom({querySelector: {'#nextSidebar .next-brand-mark': mark}});
const api = new Function(script + `
  return { applyBranding, iconValue: () => SITE_ICON_VALUE };
`)();
const lastIcon = () => mark.children[mark.children.length - 1];

api.applyBranding({site_title: '', site_icon: ''});
assert.equal(dom.byId('siteFavicon').href, '/favicon.svg');
assert.equal(lastIcon().src, '/favicon.svg');
assert.equal(dom.byId('setSiteIconPreview').src, '/favicon.svg');
assert.equal(dom.byId('setSiteIconState').textContent, '默认图标');
assert.equal(api.iconValue(), '');

const custom = 'data:image/png;base64,iVBORw0KGgo=';
api.applyBranding({site_title: 'X', site_icon: custom});
assert.equal(dom.byId('siteFavicon').href, custom);
assert.equal(lastIcon().src, custom);
assert.equal(dom.byId('setSiteIconPreview').src, custom);
assert.equal(dom.byId('setSiteIconState').textContent, '已设置图标');

api.applyBranding({site_title: '', site_icon: ''});
assert.equal(dom.byId('siteFavicon').href, '/favicon.svg');
assert.equal(lastIcon().src, '/favicon.svg');
```

Register the suite in `tests/dashboard/node/run.js` and add the file to the contract harness artifact list.

- [ ] **Step 2: Run the suite to verify it fails**

Run: `DASHBOARD_HTML=$PWD/internal/web/dashboard.html node tests/dashboard/node/_test_site_icon.js`
Expected: FAIL because the current default is `data:,` and the sidebar mark is text `m`.

- [ ] **Step 3: Implement the default-icon fallback**

In `dashboard.html`:

- Change the head link to `<link id="siteFavicon" rel="icon" type="image/svg+xml" href="/favicon.svg">`.
- Replace `<div class="next-brand-mark">m</div>` with an `<img src="/favicon.svg" alt="" decoding="async">`.
- Add `const DEFAULT_SITE_ICON = '/favicon.svg';` next to `SITE_ICON_VALUE`.
- In `setBrandIconState`, show the default preview when `SITE_ICON_VALUE` is empty and set the state text to `默认图标`; change the initial static settings label from `未设置图标` to `默认图标`.
- In `applyBranding`, use `rawIcon || DEFAULT_SITE_ICON` for both the sidebar image and the favicon; preserve the existing custom icon behavior.
- Add `'默认图标': 'Default icon'` to the English map and `"默认图标": "預設圖示"` to the HANT phrase map.

- [ ] **Step 4: Run the Node suite and dashboard tests**

Run:

```sh
DASHBOARD_HTML=$PWD/internal/web/dashboard.html node tests/dashboard/node/run.js
go test ./tests/dashboard -count=1
```

Expected: all Node suites pass; hash/oracle failures are expected until Task 3.

- [ ] **Step 5: Commit**

```bash
git add internal/web/dashboard.html tests/dashboard/node/_test_site_icon.js \
  tests/dashboard/node/run.js tests/dashboard/contract_test.go
git commit -m "feat(dashboard): use default mix2api icon"
```

### Task 3: Update pinned hashes, oracle, docs and full verification

**Files:**
- Modify: `internal/web/assets.go` (`DashboardSHA256`)
- Modify: `tests/oracle/dashboard.md`
- Modify: `tests/dashboard/contract_test.go` if route/hash assertions remain
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Compute the new dashboard hash and size**

Run:

```sh
sha256sum internal/web/dashboard.html
wc -c internal/web/dashboard.html
```

- [ ] **Step 2: Update the pin and oracle**

Set `DashboardSHA256` to the new digest. Update `tests/oracle/dashboard.md` with the new v2 hash/size, the new `/favicon.svg` row, and `/favicon.ico` returning 200.

- [ ] **Step 3: Add changelog entry**

Add an `## [Unreleased]` section at the top of `CHANGELOG.md` describing the default icon in Chinese and English.

- [ ] **Step 4: Run the full verification**

Run:

```sh
make check
make race
DASHBOARD_HTML=$PWD/internal/web/dashboard.html node tests/dashboard/node/run.js
```

Expected: all pass, including the updated dashboard hash/oracle tests and the new site-icon suite.

- [ ] **Step 5: Commit**

```bash
git add internal/web/assets.go tests/oracle/dashboard.md CHANGELOG.md
git commit -m "docs: record default icon release"
```