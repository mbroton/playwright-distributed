package browser

const (
	Chromium               = "chromium"
	Firefox                = "firefox"
	WebKit                 = "webkit"
	AllowedValuesText      = Chromium + ", " + Firefox + ", " + WebKit
	UnsupportedTypeMessage = "unsupported browser; allowed values: " + AllowedValuesText
)

func IsSupportedType(browserType string) bool {
	switch browserType {
	case Chromium, Firefox, WebKit:
		return true
	default:
		return false
	}
}
