package oauth_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/oauth"
)

func TestCallbackAcceptsExactlyOneMatchingState(t *testing.T) {
	cb, err := oauth.ListenCallback("")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	result := make(chan string, 1)
	go func() {
		code, waitErr := cb.Wait(context.Background(), "right-state")
		if waitErr != nil {
			result <- "ERROR:" + waitErr.Error()
			return
		}
		result <- code
	}()
	wrong, err := http.Get(cb.RedirectURI() + "?code=wrong&state=wrong-state")
	if err != nil {
		t.Fatal(err)
	}
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong status=%d", wrong.StatusCode)
	}
	good, err := http.Get(cb.RedirectURI() + "?code=good-code&state=right-state")
	if err != nil {
		t.Fatal(err)
	}
	good.Body.Close()
	if got := <-result; got != "good-code" {
		t.Fatalf("code=%q", got)
	}
	if _, err := http.Get(cb.RedirectURI() + "?code=second&state=right-state"); err == nil {
		t.Fatal("duplicate callback reached server after terminal result")
	}
}

func TestCallbackWrongPathAndMissingStateDoNotTerminateWait(t *testing.T) {
	cb, err := oauth.ListenCallback("")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	result := make(chan string, 1)
	go func() {
		code, waitErr := cb.Wait(context.Background(), "right-state")
		if waitErr != nil {
			result <- "ERROR:" + waitErr.Error()
			return
		}
		result <- code
	}()

	wrongPath, err := http.Get(strings.TrimSuffix(cb.RedirectURI(), "/callback") + "/wrong?code=bad&state=right-state")
	if err != nil {
		t.Fatal(err)
	}
	wrongPath.Body.Close()
	if wrongPath.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong path status=%d", wrongPath.StatusCode)
	}
	missingState, err := http.Get(cb.RedirectURI() + "?code=bad")
	if err != nil {
		t.Fatal(err)
	}
	missingState.Body.Close()
	if missingState.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing state status=%d", missingState.StatusCode)
	}
	select {
	case got := <-result:
		t.Fatalf("Wait returned before terminal callback: %q", got)
	case <-time.After(20 * time.Millisecond):
	}
	good, err := http.Get(cb.RedirectURI() + "?code=good-code&state=right-state")
	if err != nil {
		t.Fatal(err)
	}
	good.Body.Close()
	if got := <-result; got != "good-code" {
		t.Fatalf("code=%q", got)
	}
}

func TestCallbackOAuthErrorIsTerminalWithoutReflectingDescription(t *testing.T) {
	cb, err := oauth.ListenCallback("")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	result := make(chan error, 1)
	go func() {
		_, waitErr := cb.Wait(context.Background(), "right-state")
		result <- waitErr
	}()
	resp, err := http.Get(cb.RedirectURI() + "?error=access_denied&error_description=never-reflect-this&state=right-state")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "授权失败") {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	waitErr := <-result
	if waitErr == nil || strings.Contains(waitErr.Error(), "never-reflect-this") {
		t.Fatalf("Wait error=%v", waitErr)
	}
}

func TestCallbackManualModeAnswersBrowserWithoutConsumingAuthorization(t *testing.T) {
	cb, err := oauth.ListenCallback("")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	if err := cb.BeginManual("right-state"); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(cb.RedirectURI() + "?code=browser-code&state=right-state")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "粘贴到终端") || strings.Contains(string(body), "browser-code") {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}

	second, err := http.Get(cb.RedirectURI() + "?code=second-code&state=right-state")
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second manual callback status=%d", second.StatusCode)
	}
	if err := cb.BeginManual("another-state"); err == nil {
		t.Fatal("second BeginManual() error = nil")
	}
}

func TestCallbackContextTimeoutClosesListener(t *testing.T) {
	cb, err := oauth.ListenCallback("")
	if err != nil {
		t.Fatal(err)
	}
	redirectURI := cb.RedirectURI()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := cb.Wait(ctx, "right-state"); err == nil {
		t.Fatal("Wait() error = nil, want context timeout")
	}
	if _, err := http.Get(redirectURI + "?code=late&state=right-state"); err == nil {
		t.Fatal("callback listener remained open after timeout")
	}
	if err := cb.Close(); err != nil {
		t.Fatalf("Close after timeout: %v", err)
	}
}

func TestCallbackConcurrentTerminalRequestsConsumeOneResult(t *testing.T) {
	cb, err := oauth.ListenCallback("")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	result := make(chan string, 1)
	go func() {
		code, waitErr := cb.Wait(context.Background(), "right-state")
		if waitErr != nil {
			result <- "ERROR:" + waitErr.Error()
			return
		}
		result <- code
	}()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	var requests sync.WaitGroup
	requests.Add(2)
	for _, code := range []string{"first", "second"} {
		go func() {
			defer requests.Done()
			resp, requestErr := client.Get(cb.RedirectURI() + "?code=" + code + "&state=right-state")
			if requestErr == nil {
				resp.Body.Close()
			}
		}()
	}
	requests.Wait()
	select {
	case got := <-result:
		if got != "first" && got != "second" {
			t.Fatalf("terminal result=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return a terminal result")
	}
	select {
	case extra := <-result:
		t.Fatalf("second terminal result=%q", extra)
	default:
	}
	if err := cb.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestListenCallbackRejectsUnsafeSavedRedirectURI(t *testing.T) {
	tests := []struct {
		name string
		uri  string
	}{
		{"bad scheme", "https://127.0.0.1:12345/callback"},
		{"bad hostname", "http://localhost:12345/callback"},
		{"wildcard hostname", "http://0.0.0.0:12345/callback"},
		{"ipv6 hostname", "http://[::1]:12345/callback"},
		{"missing port", "http://127.0.0.1/callback"},
		{"zero port", "http://127.0.0.1:0/callback"},
		{"nonnumeric port", "http://127.0.0.1:http/callback"},
		{"wrong path", "http://127.0.0.1:12345/other"},
		{"userinfo", "http://user@127.0.0.1:12345/callback"},
		{"query", "http://127.0.0.1:12345/callback?x=1"},
		{"fragment", "http://127.0.0.1:12345/callback#x"},
		{"empty fragment", "http://127.0.0.1:12345/callback#"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cb, err := oauth.ListenCallback(tc.uri)
			if err == nil {
				cb.Close()
				t.Fatalf("ListenCallback(%q) error = nil", tc.uri)
			}
		})
	}
}

func TestListenCallbackBindsExactSavedRedirectURI(t *testing.T) {
	reserved, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	want := "http://" + address + "/callback"
	cb, err := oauth.ListenCallback(want)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	if cb.RedirectURI() != want {
		t.Fatalf("RedirectURI()=%q, want %q", cb.RedirectURI(), want)
	}
}
