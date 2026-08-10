package httpx

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRedactHeadersRemovesCredentials(t *testing.T) {
	headers := http.Header{
		"Authorization": {"Bearer bearer-secret"},
		"Cookie":        {"session=cookie-secret"},
		"Set-Cookie":    {"session=set-cookie-secret"},
		"X-Amz-Date":    {"amz-secret"},
		"Accept":        {"application/json"},
	}
	redacted := RedactHeaders(headers)
	for _, key := range []string{"Authorization", "Cookie", "Set-Cookie", "X-Amz-Date"} {
		if redacted.Get(key) != "" {
			t.Fatalf("RedactHeaders() retained %s", key)
		}
	}
	if got := redacted.Get("Accept"); got != "application/json" {
		t.Fatalf("RedactHeaders() Accept = %q, want application/json", got)
	}
	if headers.Get("Authorization") == "" {
		t.Fatal("RedactHeaders() modified the input headers")
	}
}

func TestRedactURLReplacesSensitiveQueryValues(t *testing.T) {
	u, err := url.Parse("https://drive.example/download/report?part=2&token=token-secret&Signature=signature-secret&X-Amz-Credential=credential-secret&accessKey=access-secret&code=oauth-code-secret&state=oauth-state-secret&code_verifier=oauth-verifier-secret&error_description=oauth-description-secret&keep=yes")
	if err != nil {
		t.Fatal(err)
	}
	got := RedactURL(u)
	if containsAny(got, "token-secret", "signature-secret", "credential-secret", "access-secret", "oauth-code-secret", "oauth-state-secret", "oauth-verifier-secret", "oauth-description-secret") {
		t.Fatalf("RedactURL() leaked a secret in %q", got)
	}
	for _, want := range []string{"https://drive.example/download/report", "part=2", "keep=yes", "%5BREDACTED%5D"} {
		if !strings.Contains(got, want) {
			t.Fatalf("RedactURL() = %q, missing %q", got, want)
		}
	}
}

func TestDecodeJSONRejectsTrailingNonWhitespace(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(`{"ok":true} trailing`))}
	var dst struct {
		OK bool `json:"ok"`
	}
	if err := DecodeJSON(resp, 0, &dst); err == nil {
		t.Fatal("DecodeJSON() error = nil, want trailing-data rejection")
	}
}

func TestDecodeJSONUsesDefaultEightMiBLimit(t *testing.T) {
	tooLarge := `"` + strings.Repeat("a", 8<<20) + `"`
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(tooLarge))}
	var dst string
	if err := DecodeJSON(resp, 0, &dst); err == nil {
		t.Fatal("DecodeJSON() error = nil, want default size-limit rejection")
	}
}

func TestDecodeJSONClosesConsumedBody(t *testing.T) {
	body := &closeTrackedBody{Reader: strings.NewReader(`{"ok":true}`)}
	resp := &http.Response{Body: body}
	var dst struct {
		OK bool `json:"ok"`
	}
	if err := DecodeJSON(resp, 0, &dst); err != nil {
		t.Fatal(err)
	}
	if !body.closed {
		t.Fatal("DecodeJSON() did not close the consumed response body")
	}
}
