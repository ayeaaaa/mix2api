// Package update implements stable release discovery and the validated update
// request bridge. It never downloads, replaces, or restarts application files.
package update

import (
	"regexp"
	"strconv"
	"strings"
)

var versionPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// ParseVersion accepts strict stable vX.Y.Z / X.Y.Z tags only.
func ParseVersion(text string) (major, minor, patch int, ok bool) {
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(match[1])
	minor, _ = strconv.Atoi(match[2])
	patch, _ = strconv.Atoi(match[3])
	return major, minor, patch, true
}

// IsNewer reports strict version ordering.
func IsNewer(candidate, current string) bool {
	leftMajor, leftMinor, leftPatch, leftOK := ParseVersion(candidate)
	rightMajor, rightMinor, rightPatch, rightOK := ParseVersion(current)
	if !leftOK || !rightOK {
		return false
	}
	if leftMajor != rightMajor {
		return leftMajor > rightMajor
	}
	if leftMinor != rightMinor {
		return leftMinor > rightMinor
	}
	return leftPatch > rightPatch
}

// SafeReleaseURL only accepts an https GitHub release page.
func SafeReleaseURL(value string) string {
	text := strings.TrimSpace(value)
	if !strings.HasPrefix(text, "https://github.com/") {
		return ""
	}
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

// Release is one picked GitHub release.
type Release struct {
	Version     string
	Tag         string
	URL         string
	PublishedAt string
	Newer       bool
}

// PickLatest picks the highest stable version from a GitHub releases document,
// skipping drafts, prereleases and malformed tags.
func PickLatest(releases []any, currentVersion string) *Release {
	var bestVersion [3]int
	var best map[string]any
	for _, raw := range releases {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if draft, _ := entry["draft"].(bool); draft {
			continue
		}
		if prerelease, _ := entry["prerelease"].(bool); prerelease {
			continue
		}
		major, minor, patch, ok := ParseVersion(stringValue(entry["tag_name"]))
		if !ok {
			continue
		}
		if best == nil {
			best, bestVersion = entry, [3]int{major, minor, patch}
			continue
		}
		candidate := [3]int{major, minor, patch}
		if candidate[0] > bestVersion[0] ||
			(candidate[0] == bestVersion[0] && candidate[1] > bestVersion[1]) ||
			(candidate[0] == bestVersion[0] && candidate[1] == bestVersion[1] && candidate[2] > bestVersion[2]) {
			best, bestVersion = entry, candidate
		}
	}
	if best == nil {
		return nil
	}
	version := strconv.Itoa(bestVersion[0]) + "." + strconv.Itoa(bestVersion[1]) + "." + strconv.Itoa(bestVersion[2])
	tag := strings.TrimSpace(stringValue(best["tag_name"]))
	if len(tag) > 64 {
		tag = tag[:64]
	}
	return &Release{
		Version: version, Tag: tag,
		URL:         SafeReleaseURL(stringValue(best["html_url"])),
		PublishedAt: truncateString(strings.TrimSpace(stringValue(best["published_at"])), 32),
		Newer:       IsNewer(version, currentVersion),
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func truncateString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
