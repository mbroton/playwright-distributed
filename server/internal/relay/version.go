package relay

import "regexp"

var (
	userAgentVersionPattern = regexp.MustCompile(`Playwright/([0-9]+)\.([0-9]+)(\.[0-9]+)?([[:space:]]|$)`)
	inputVersionPattern     = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?)?$`)
)

func UserAgentVersion(userAgent string) (version, prefix string, ok bool) {
	matches := userAgentVersionPattern.FindStringSubmatch(userAgent)
	if matches == nil {
		return "", "", false
	}
	return matches[1] + "." + matches[2], matches[1] + "." + matches[2] + ".", true
}

func VersionPrefix(version string) (string, bool) {
	matches := inputVersionPattern.FindStringSubmatch(version)
	if matches == nil {
		return "", false
	}
	return matches[1] + "." + matches[2] + ".", true
}
