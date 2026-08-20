package sharelink

import (
	"net/url"
	"testing"
)

func TestSameOriginTreatsDefaultHTTPSPortAsEquivalent(t *testing.T) {
	withoutPort, _ := url.Parse("https://disk.example")
	withDefaultPort, _ := url.Parse("https://DISK.example:443")
	withOtherPort, _ := url.Parse("https://disk.example:444")
	if !sameOrigin(withoutPort, withDefaultPort) {
		t.Fatal("default HTTPS port should be the same origin")
	}
	if sameOrigin(withoutPort, withOtherPort) {
		t.Fatal("non-default port should be a different origin")
	}
}

func TestParseLandingRejectsDuplicateRecognizedParameters(t *testing.T) {
	for _, key := range []string{"type", "item_type", "title", "expires_at", "password_required", "verify_mobile"} {
		values := url.Values{key: []string{"first", "second"}}
		_, err := parseLanding(values)
		if err == nil {
			t.Fatalf("parseLanding duplicate %s error = nil", key)
		}
	}
}
