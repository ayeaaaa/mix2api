package workbuddy

import (
	"crypto/sha256"
	"fmt"
	"net/http"
)

// webBrowserProfile mirrors v1's per-account browser identity. All accounts
// must not share one UA/platform: the official web client is a real browser,
// and a single gateway-wide UA is a stable automation signal.
type webBrowserProfile struct {
	UA       string
	Platform string
	Chrome   string
	Brand    string
	Mobile   string
}

var webBrowserProfiles = []webBrowserProfile{
	{
		UA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0",
		Platform: "Windows", Chrome: "140", Brand: "Microsoft Edge", Mobile: "?0",
	},
	{
		UA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
		Platform: "Windows", Chrome: "140", Brand: "Google Chrome", Mobile: "?0",
	},
	{
		UA: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
		Platform: "macOS", Chrome: "140", Brand: "Google Chrome", Mobile: "?0",
	},
	{
		UA: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0",
		Platform: "macOS", Chrome: "140", Brand: "Microsoft Edge", Mobile: "?0",
	},
}

// webProfile selects one profile deterministically per account. It mirrors
// v1's sha256("web-profile:"+uid)[0] selection.
func webProfile(uid string) webBrowserProfile {
	if uid == "" {
		uid = "anonymous"
	}
	sum := sha256.Sum256([]byte("web-profile:" + uid))
	return webBrowserProfiles[int(sum[0])%len(webBrowserProfiles)]
}

// webBrowserHeaders returns the browser-side headers for one account. It does
// not include credentials; the caller adds bearer and X-User-Id.
func webBrowserHeaders(uid, realm string) http.Header {
	profile := webProfile(uid)
	language := "en-US,en;q=0.9"
	if realm == realmCN {
		language = "zh-CN,zh;q=0.9,en;q=0.8"
	}
	headers := http.Header{}
	headers.Set("User-Agent", profile.UA)
	headers.Set("Accept-Language", language)
	headers.Set("sec-ch-ua", fmt.Sprintf(`"Chromium";v="%s", "Not=A?Brand";v="24", "%s";v="%s"`,
		profile.Chrome, profile.Brand, profile.Chrome))
	headers.Set("sec-ch-ua-mobile", profile.Mobile)
	headers.Set("sec-ch-ua-platform", fmt.Sprintf(`"%s"`, profile.Platform))
	headers.Set("sec-fetch-site", "same-origin")
	headers.Set("sec-fetch-mode", "cors")
	headers.Set("sec-fetch-dest", "empty")
	return headers
}
