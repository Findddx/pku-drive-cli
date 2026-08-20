package sharelink

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// linkTokenTransport handles AnyShare's legacy colon-bearing cookie name,
// which net/http/cookiejar intentionally rejects as an RFC cookie name. The
// value remains in memory and is attached only to the configured HTTPS origin.
type linkTokenTransport struct {
	base       http.RoundTripper
	origin     *url.URL
	cookieName string

	mu    sync.RWMutex
	token string
}

func (t *linkTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	attempt := req
	if sameOrigin(req.URL, t.origin) {
		if token := t.Token(); token != "" {
			attempt = req.Clone(req.Context())
			attempt.Header = req.Header.Clone()
			attempt.Header.Add("Cookie", t.cookieName+"="+token)
		}
	}
	resp, err := t.base.RoundTrip(attempt)
	if err != nil || resp == nil || !sameOrigin(req.URL, t.origin) {
		return resp, err
	}
	for _, raw := range resp.Header.Values("Set-Cookie") {
		if token, ok := parseLegacyLinkCookie(raw, t.cookieName); ok {
			t.mu.Lock()
			t.token = token
			t.mu.Unlock()
		}
	}
	return resp, nil
}

func (t *linkTokenTransport) Token() string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.token
}

func parseLegacyLinkCookie(raw, wantName string) (string, bool) {
	pair, _, _ := strings.Cut(raw, ";")
	name, value, ok := strings.Cut(pair, "=")
	if !ok || strings.TrimSpace(name) != wantName {
		return "", false
	}
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			return "", false
		}
		value = unquoted
	}
	if value == "" || len(value) > 16<<10 || strings.ContainsAny(value, "\r\n;") {
		return "", false
	}
	return value, true
}
