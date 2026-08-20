package output_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/output"
)

func TestRendererJSONSuccessIsOneRedactedObject(t *testing.T) {
	// Mutation caught: omitting the success envelope, emitting multiple values, or walking only the top level during redaction.
	var stdout, stderr bytes.Buffer
	renderer := output.NewRenderer(&stdout, &stderr, true)
	err := renderer.Success("status", map[string]any{
		"account": "alice",
		"nested": map[string]any{
			"access_token":  "fake-access-token",
			"Authorization": "Bearer fake-bearer",
			"response-body": "fake-response-body",
			"raw_error":     "fake-nested-error",
			"children":      []any{map[string]any{"client-secret": "fake-client-secret", "cookie": "fake-cookie"}},
		},
		"link": "https://objects.example.invalid/file?X-Amz-Signature=fake-signature&access_token=fake-query-token",
	}, "ignored human")
	if err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
	for _, secret := range []string{"fake-access-token", "fake-bearer", "fake-response-body", "fake-nested-error", "fake-client-secret", "fake-cookie", "fake-signature", "fake-query-token"} {
		if strings.Contains(stdout.String(), secret) {
			t.Fatalf("secret %q leaked in %q", secret, stdout.String())
		}
	}
	decoder := json.NewDecoder(strings.NewReader(stdout.String()))
	var got map[string]any
	if err := decoder.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["ok"] != true || got["operation"] != "status" || got["account"] != "alice" {
		t.Fatalf("object=%v", got)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("extra JSON value in %q", stdout.String())
	}
}

func TestRendererJSONFailureHasOnlySafeEnvelope(t *testing.T) {
	// Mutation caught: including operation/raw causes or leaving credentials, headers, cookies, or signed query values in a failure message.
	var stdout, stderr bytes.Buffer
	renderer := output.NewRenderer(&stdout, &stderr, true)
	message := "request Authorization: Bearer fake-bearer Cookie=fake-cookie client_secret=fake-client https://objects.invalid/a?signature=fake-signature&token=fake-token response body:\n{\"credential\":\"fake-multiline-body\",\n\"access_token\":\"fake-body-token\"}"
	if err := renderer.Failure("network", message); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["ok"] != false || got["category"] != "network" {
		t.Fatalf("failure=%v", got)
	}
	for _, secret := range []string{"fake-bearer", "fake-cookie", "fake-client", "fake-signature", "fake-token", "fake-multiline-body", "fake-body-token", "objects.invalid"} {
		if strings.Contains(stdout.String(), secret) {
			t.Fatalf("secret %q leaked in %q", secret, stdout.String())
		}
	}
}

func TestRendererHumanStreams(t *testing.T) {
	// Mutation caught: directing human success/errors to the wrong streams or including an unsafe nested cause supplied after the stable message.
	var stdout, stderr bytes.Buffer
	renderer := output.NewRenderer(&stdout, &stderr, false)
	if err := renderer.Success("mkdir", nil, "created /docs"); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Failure("local", "local operation failed: access_token=fake-token"); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "created /docs\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "local operation failed") || strings.Contains(stderr.String(), "fake-token") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestTerminalTextEscapesTerminalControls(t *testing.T) {
	input := "报告\t\x1b]8;;https://evil.invalid\a名称\n.tsv\u009b31m"
	got := output.TerminalText(input)
	for _, character := range got {
		if character < 0x20 || character >= 0x7f && character <= 0x9f {
			t.Fatalf("terminal control U+%04X remained in %q", character, got)
		}
	}
	for _, want := range []string{`\x09`, `\x1B`, `\x07`, `\x0A`, `\x9B`} {
		if !strings.Contains(got, want) {
			t.Fatalf("TerminalText(%q) = %q, missing %q", input, got, want)
		}
	}
	if !strings.Contains(got, "报告") || !strings.Contains(got, "名称") {
		t.Fatalf("ordinary Unicode was lost: %q", got)
	}
}

func TestRedactMessageRemovesOAuthCallbackParameters(t *testing.T) {
	message := "callback failed at http://127.0.0.1:43123/callback?code=fake-oauth-code&state=fake-oauth-state&code_verifier=fake-oauth-verifier&error_description=fake-oauth-description"
	got := output.RedactMessage(message)
	for _, secret := range []string{"fake-oauth-code", "fake-oauth-state", "fake-oauth-verifier", "fake-oauth-description"} {
		if strings.Contains(got, secret) {
			t.Fatalf("OAuth parameter %q leaked in %q", secret, got)
		}
	}
}

func TestRedactMessageRemovesShareCapabilities(t *testing.T) {
	for _, message := range []string{
		"share failed at https://disk.pku.edu.cn/link/AA-ShareCapabilityMarker",
		"legacy share https://disk.pku.edu.cn/anyshare/#/link/AR_ShareCapabilityMarker",
		"metadata https://disk.pku.edu.cn/api/shared-link/v1/links/value?link_id=ShareCapabilityMarker",
	} {
		got := output.RedactMessage(message)
		if strings.Contains(got, "ShareCapabilityMarker") || strings.Contains(got, "/link/AA-") || strings.Contains(got, "/link/AR_") {
			t.Fatalf("share capability leaked in %q", got)
		}
	}
}

func TestRendererFailureRedactsCompleteCredentialHeaders(t *testing.T) {
	tests := []struct {
		name     string
		jsonMode bool
		message  string
	}{
		{
			name:     "JSON LF mixed-case headers",
			jsonMode: true,
			message:  "request failed cOoKiE: session=fake-cookie-one; refresh=fake-cookie-two\n\tcontinuation-token=fake-cookie-three\nunrelated diagnostic\nAuthorization: Digest username=fake-user, response=fake-auth-signature\n more-token=fake-auth-continuation\nlast diagnostic",
		},
		{
			name:     "human CRLF mixed-case headers",
			jsonMode: false,
			message:  "prefix aUtHoRiZaTiOn: Digest username=fake-user, nonce=fake-nonce\r\n\tresponse=fake-auth-response\r\nnext diagnostic\r\nCookie: a=fake-cookie-one; b=fake-cookie-two\r\n  continuation=fake-cookie-continuation\r\nlast diagnostic",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Mutation caught: redacting only the first whitespace-delimited header token or consuming a following unrelated diagnostic line.
			var stdout, stderr bytes.Buffer
			renderer := output.NewRenderer(&stdout, &stderr, test.jsonMode)
			if err := renderer.Failure("network", test.message); err != nil {
				t.Fatal(err)
			}
			rendered := stderr.String()
			if test.jsonMode {
				var envelope map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				rendered, _ = envelope["message"].(string)
			}
			for _, forbidden := range []string{"fake-cookie-one", "fake-cookie-two", "fake-cookie-three", "fake-cookie-continuation", "fake-user", "fake-nonce", "fake-auth-signature", "fake-auth-continuation", "fake-auth-response"} {
				if strings.Contains(rendered, forbidden) {
					t.Fatalf("credential %q leaked in %q", forbidden, rendered)
				}
			}
			for _, preserved := range []string{"unrelated diagnostic", "last diagnostic"} {
				if !strings.Contains(test.message, preserved) {
					continue
				}
				if !strings.Contains(rendered, preserved) {
					t.Fatalf("unrelated line %q was swallowed in %q", preserved, rendered)
				}
			}
		})
	}
}

func TestRendererJSONFailureRedactsEveryAbsoluteURL(t *testing.T) {
	// Mutation caught: treating only signed/tokenized URLs as unsafe in the JSON failure envelope.
	var stdout, stderr bytes.Buffer
	renderer := output.NewRenderer(&stdout, &stderr, true)
	message := "request failed at https://control.example.invalid/plain/path; mirror ftp://objects.example.invalid/file\nnext diagnostic"
	if err := renderer.Failure("network", message); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	rendered, _ := envelope["message"].(string)
	for _, forbidden := range []string{"https://", "ftp://", "control.example.invalid", "objects.example.invalid", "/plain/path"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("URL material %q leaked in %q", forbidden, rendered)
		}
	}
	for _, preserved := range []string{"request failed at", "mirror", "next diagnostic"} {
		if !strings.Contains(rendered, preserved) {
			t.Fatalf("diagnostic %q missing from %q", preserved, rendered)
		}
	}
}
