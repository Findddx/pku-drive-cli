package oauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

const maximumPastedCallbackBytes = 16 << 10

var errInvalidPastedCallback = errors.New("callback response is invalid")

func waitForPastedCallback(ctx context.Context, reader *callbackLineReader, redirectURI, expectedState string, notice io.Writer) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		raw, err := reader.ReadLine(
			ctx,
			notice,
			"Paste the complete callback URL (input hidden), then press Enter: ",
			"Paste the complete callback URL (input may be visible; use ssh -t for hidden input), then press Enter: ",
		)
		if err != nil {
			if errors.Is(err, errCallbackInputRestore) {
				return "", authError("callback", "restore callback input", errCallbackInputRestore)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", authError("callback", "wait canceled", ctxErr)
			}
			return "", authError("callback", "read pasted callback URL", errors.New("callback input failed"))
		}
		code, err := parsePastedCallback(raw, redirectURI, expectedState)
		if err == nil || !errors.Is(err, errInvalidPastedCallback) {
			return code, err
		}
		if _, writeErr := fmt.Fprintln(notice, "Invalid callback URL; paste the complete URL from the current browser attempt."); writeErr != nil {
			return "", authError("callback", "write callback prompt", errors.New("callback prompt failed"))
		}
	}
	return "", invalidPastedCallback()
}

func parsePastedCallback(raw, redirectURI, expectedState string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maximumPastedCallbackBytes || strings.Contains(raw, "#") || containsControl(raw) {
		return "", invalidPastedCallback()
	}
	expected, err := url.Parse(redirectURI)
	if err != nil {
		return "", invalidPastedCallback()
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Scheme != expected.Scheme || parsed.Host != expected.Host || parsed.Path != expected.Path || parsed.EscapedPath() != expected.EscapedPath() || parsed.Opaque != "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.ForceQuery {
		return "", invalidPastedCallback()
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query["state"]) != 1 || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(expectedState)) != 1 {
		return "", invalidPastedCallback()
	}
	codes := query["code"]
	oauthErrors := query["error"]
	if len(codes) == 1 && codes[0] != "" && !containsControl(codes[0]) && len(oauthErrors) == 0 {
		return codes[0], nil
	}
	if len(oauthErrors) == 1 && oauthErrors[0] != "" && len(codes) == 0 {
		descriptions := query["error_description"]
		description := ""
		if len(descriptions) == 1 {
			description = descriptions[0]
		} else if len(descriptions) > 1 {
			return "", invalidPastedCallback()
		}
		if explicitUnsupportedPKCE(oauthErrors[0], description) {
			return "", authError("authorize", "authorization server rejected PKCE", errPKCEUnsupported)
		}
		return "", authError("authorize", "authorization server returned an error", errors.New("authorization denied"))
	}
	return "", invalidPastedCallback()
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func invalidPastedCallback() error {
	return authError("callback", "invalid pasted callback URL", errInvalidPastedCallback)
}
