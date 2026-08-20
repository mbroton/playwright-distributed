package relay

import "testing"

func TestUserAgentVersion(t *testing.T) {
	tests := []struct {
		name        string
		userAgent   string
		wantVersion string
		wantPrefix  string
		wantOK      bool
	}{
		{
			name:        "full version",
			userAgent:   "Playwright/1.62.1 (x64; linux)",
			wantVersion: "1.62",
			wantPrefix:  "1.62.",
			wantOK:      true,
		},
		{
			name:        "major and minor",
			userAgent:   "client Playwright/2.7",
			wantVersion: "2.7",
			wantPrefix:  "2.7.",
			wantOK:      true,
		},
		{name: "missing", userAgent: "Mozilla/5.0"},
		{name: "unparseable", userAgent: "Playwright/latest"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			version, prefix, ok := UserAgentVersion(test.userAgent)
			if version != test.wantVersion || prefix != test.wantPrefix || ok != test.wantOK {
				t.Fatalf(
					"UserAgentVersion(%q) = %q, %q, %t; want %q, %q, %t",
					test.userAgent,
					version,
					prefix,
					ok,
					test.wantVersion,
					test.wantPrefix,
					test.wantOK,
				)
			}
		})
	}
}

func TestVersionPrefix(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		wantPrefix string
		wantOK     bool
	}{
		{name: "major and minor", version: "1.62", wantPrefix: "1.62.", wantOK: true},
		{name: "semver", version: "1.62.1-beta.1+build", wantPrefix: "1.62.", wantOK: true},
		{name: "missing minor", version: "1", wantOK: false},
		{name: "extra numeric component", version: "1.62.1.2", wantOK: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prefix, ok := VersionPrefix(test.version)
			if prefix != test.wantPrefix || ok != test.wantOK {
				t.Fatalf(
					"VersionPrefix(%q) = %q, %t; want %q, %t",
					test.version,
					prefix,
					ok,
					test.wantPrefix,
					test.wantOK,
				)
			}
		})
	}
}
