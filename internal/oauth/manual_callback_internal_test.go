package oauth

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestParsePastedCallbackAcceptsExactOneTimeResponse(t *testing.T) {
	redirect := "http://127.0.0.1:43123/callback"
	raw := redirect + "?code=fake-code&scope=offline+openid+all&state=fake-state"
	code, err := parsePastedCallback(raw, redirect, "fake-state")
	if err != nil {
		t.Fatal(err)
	}
	if code != "fake-code" {
		t.Fatalf("code = %q", code)
	}
}

func TestParsePastedCallbackAcceptsCRLFAndHarmlessExtraParameters(t *testing.T) {
	redirect := "http://127.0.0.1:43123/callback"
	raw := "\r\n" + redirect + "?scope=offline+openid+all&code=fake-code&state=fake-state\r\n"
	code, err := parsePastedCallback(raw, redirect, "fake-state")
	if err != nil || code != "fake-code" {
		t.Fatalf("code=%q err=%v", code, err)
	}
}

func TestParsePastedCallbackRejectsMismatchedOrAmbiguousResponse(t *testing.T) {
	redirect := "http://127.0.0.1:43123/callback"
	tests := []struct {
		name string
		raw  string
	}{
		{name: "wrong host", raw: "http://localhost:43123/callback?code=x&state=fake-state"},
		{name: "wrong port", raw: "http://127.0.0.1:43124/callback?code=x&state=fake-state"},
		{name: "wrong path", raw: "http://127.0.0.1:43123/other?code=x&state=fake-state"},
		{name: "https", raw: "https://127.0.0.1:43123/callback?code=x&state=fake-state"},
		{name: "fragment", raw: redirect + "?code=x&state=fake-state#fragment"},
		{name: "empty fragment", raw: redirect + "?code=x&state=fake-state#"},
		{name: "userinfo", raw: "http://user@127.0.0.1:43123/callback?code=x&state=fake-state"},
		{name: "encoded path", raw: "http://127.0.0.1:43123/%63allback?code=x&state=fake-state"},
		{name: "wrong state", raw: redirect + "?code=x&state=other"},
		{name: "duplicate state", raw: redirect + "?code=x&state=fake-state&state=fake-state"},
		{name: "duplicate code", raw: redirect + "?code=x&code=y&state=fake-state"},
		{name: "code and error", raw: redirect + "?code=x&error=access_denied&state=fake-state"},
		{name: "duplicate error", raw: redirect + "?error=access_denied&error=invalid_request&state=fake-state"},
		{name: "duplicate error description", raw: redirect + "?error=access_denied&error_description=one&error_description=two&state=fake-state"},
		{name: "missing result", raw: redirect + "?scope=openid&state=fake-state"},
		{name: "malformed query", raw: redirect + "?code=x;state=fake-state"},
		{name: "encoded NUL in code", raw: redirect + "?code=%00&state=fake-state"},
		{name: "encoded control in code", raw: redirect + "?code=%1f&state=fake-state"},
		{name: "literal control", raw: redirect + "?code=x\t&state=fake-state"},
		{name: "literal carriage return", raw: redirect + "?co\rde=x&state=fake-state"},
		{name: "too long", raw: redirect + "?code=" + strings.Repeat("x", maximumPastedCallbackBytes) + "&state=fake-state"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code, err := parsePastedCallback(test.raw, redirect, "fake-state"); err == nil || code != "" {
				t.Fatalf("code=%q err=%v", code, err)
			}
		})
	}
}

func TestParsePastedCallbackMapsOAuthErrorsWithoutLeakingDescription(t *testing.T) {
	redirect := "http://127.0.0.1:43123/callback"
	raw := redirect + "?error=access_denied&error_description=" + url.QueryEscape("sensitive-description") + "&state=fake-state"
	_, err := parsePastedCallback(raw, redirect, "fake-state")
	if err == nil || strings.Contains(err.Error(), "sensitive-description") {
		t.Fatalf("error = %v", err)
	}

	pkce := redirect + "?error=invalid_request&error_description=" + url.QueryEscape("PKCE code challenge is not supported") + "&state=fake-state"
	_, err = parsePastedCallback(pkce, redirect, "fake-state")
	if !errors.Is(err, errPKCEUnsupported) {
		t.Fatalf("error = %v, want PKCE unsupported", err)
	}
}

func TestWaitForPastedCallbackRetriesWithoutReflectingInvalidInput(t *testing.T) {
	redirect := "http://127.0.0.1:43123/callback"
	const invalidCode = "invalid-code-never-reflect"
	input := strings.NewReader(
		redirect + "?code=" + invalidCode + "&state=old-state\n" +
			redirect + "?code=valid-code&state=current-state\n",
	)
	var notice bytes.Buffer
	code, err := waitForPastedCallback(context.Background(), newCallbackLineReader(input), redirect, "current-state", &notice)
	if err != nil || code != "valid-code" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	if strings.Contains(notice.String(), invalidCode) || !strings.Contains(notice.String(), "Invalid callback URL") {
		t.Fatalf("notice=%q", notice.String())
	}
}
