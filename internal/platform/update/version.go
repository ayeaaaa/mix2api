// Package update implements release discovery and the validated update
// request bridge. It never downloads, replaces, or restarts application files.
package update

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	stableVersionPattern  = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)
	releaseVersionPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)
)

type parsedRelease struct {
	major, minor, patch int
	pre                 []string
}

// ParseVersion accepts strict stable vX.Y.Z / X.Y.Z tags only.
func ParseVersion(text string) (major, minor, patch int, ok bool) {
	match := stableVersionPattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(match[1])
	minor, _ = strconv.Atoi(match[2])
	patch, _ = strconv.Atoi(match[3])
	return major, minor, patch, true
}

// IsReleaseVersion accepts stable or prerelease X.Y.Z[-pre] values.
func IsReleaseVersion(text string) bool {
	_, ok := parseReleaseVersion(text)
	return ok
}

// NormalizeVersion strips a leading v and surrounding whitespace.
func NormalizeVersion(text string) string {
	return strings.TrimPrefix(strings.TrimSpace(text), "v")
}

func parseReleaseVersion(text string) (parsedRelease, bool) {
	match := releaseVersionPattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return parsedRelease{}, false
	}
	var value parsedRelease
	value.major, _ = strconv.Atoi(match[1])
	value.minor, _ = strconv.Atoi(match[2])
	value.patch, _ = strconv.Atoi(match[3])
	if match[4] != "" {
		value.pre = strings.Split(match[4], ".")
		for _, part := range value.pre {
			if part == "" {
				return parsedRelease{}, false
			}
		}
	}
	return value, true
}

// IsNewer reports semantic version ordering including prereleases. The
// running version may be a prerelease (2.0.0-rc.1), so a final 2.0.0 is newer.
func IsNewer(candidate, current string) bool {
	left, leftOK := parseReleaseVersion(candidate)
	if !leftOK {
		return false
	}
	right, rightOK := parseReleaseVersion(current)
	if !rightOK {
		right, rightOK = parseReleaseVersion(baseVersion(current))
	}
	if !rightOK {
		return false
	}
	return compareRelease(left, right) > 0
}

func compareRelease(left, right parsedRelease) int {
	if left.major != right.major {
		return compareInt(left.major, right.major)
	}
	if left.minor != right.minor {
		return compareInt(left.minor, right.minor)
	}
	if left.patch != right.patch {
		return compareInt(left.patch, right.patch)
	}
	return comparePrerelease(left.pre, right.pre)
}

func compareInt(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

// comparePrerelease implements semver prerelease ordering: a final release is
// greater than any prerelease; numeric identifiers compare numerically and are
// lower than alphanumeric identifiers; a shorter equal prefix is lower.
func comparePrerelease(left, right []string) int {
	if len(left) == 0 && len(right) == 0 {
		return 0
	}
	if len(left) == 0 {
		return 1
	}
	if len(right) == 0 {
		return -1
	}
	for index := 0; index < len(left) && index < len(right); index++ {
		leftNumber, leftNumeric := numericIdentifier(left[index])
		rightNumber, rightNumeric := numericIdentifier(right[index])
		switch {
		case leftNumeric && rightNumeric:
			if comparison := compareInt(leftNumber, rightNumber); comparison != 0 {
				return comparison
			}
		case leftNumeric != rightNumeric:
			if leftNumeric {
				return -1
			}
			return 1
		default:
			if comparison := strings.Compare(left[index], right[index]); comparison != 0 {
				return comparison
			}
		}
	}
	return compareInt(len(left), len(right))
}

func numericIdentifier(value string) (int, bool) {
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, false
		}
	}
	number, err := strconv.Atoi(value)
	return number, err == nil
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

func baseVersion(value string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if index := strings.IndexAny(value, "-+ "); index >= 0 {
		value = value[:index]
	}
	return value
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
