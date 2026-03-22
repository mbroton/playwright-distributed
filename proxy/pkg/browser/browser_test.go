package browser

import "testing"

func TestIsSupportedType(t *testing.T) {
	tests := []struct {
		name        string
		browserType string
		expect      bool
	}{
		{name: "chromium", browserType: Chromium, expect: true},
		{name: "firefox", browserType: Firefox, expect: true},
		{name: "webkit", browserType: WebKit, expect: true},
		{name: "empty", browserType: "", expect: false},
		{name: "unsupported", browserType: "opera", expect: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSupportedType(tc.browserType); got != tc.expect {
				t.Fatalf("expected %v for %q, got %v", tc.expect, tc.browserType, got)
			}
		})
	}
}
