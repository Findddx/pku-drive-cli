package oauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/httpx"
	"github.com/Findddx/pku-drive-cli/internal/oauth"
)

type browserFunc func(context.Context, string) error

func (f browserFunc) Open(ctx context.Context, rawURL string) error { return f(ctx, rawURL) }

func TestSystemBrowserPassesURLAsOneArgument(t *testing.T) {
	var name string
	var args []string
	browser := &oauth.SystemBrowser{
		LookupEnv: func(key string) (string, bool) { return ":11", key == "DISPLAY" },
		Command: func(_ context.Context, gotName string, gotArgs ...string) error {
			name, args = gotName, append([]string(nil), gotArgs...)
			return nil
		},
	}
	rawURL := "https://disk.pku.edu.cn/oauth2/auth?state=a%20b&x=%24HOME"
	if err := browser.Open(context.Background(), rawURL); err != nil {
		t.Fatal(err)
	}
	if name != "xdg-open" || !reflect.DeepEqual(args, []string{rawURL}) {
		t.Fatalf("name=%q args=%q", name, args)
	}
}

func TestSystemBrowserSkipsLaunchWithoutGraphicalDisplay(t *testing.T) {
	called := false
	browser := &oauth.SystemBrowser{
		LookupEnv: func(string) (string, bool) { return "", false },
		Command:   func(context.Context, string, ...string) error { called = true; return nil },
	}
	if err := browser.Open(context.Background(), "https://disk.example/oauth2/auth"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("browser command ran without DISPLAY or WAYLAND_DISPLAY")
	}
}

func TestSystemBrowserLaunchFailureFallsBackToManualAuthorization(t *testing.T) {
	launchErr := errors.New("synthetic xdg-open failure")
	var warning bytes.Buffer
	browser := &oauth.SystemBrowser{
		LookupEnv: func(key string) (string, bool) { return ":11", key == "DISPLAY" },
		Command:   func(context.Context, string, ...string) error { return launchErr },
		Warning:   &warning,
	}
	if err := browser.Open(context.Background(), "https://disk.example/oauth2/auth"); err != nil {
		t.Fatalf("Open() error = %v, want manual fallback", err)
	}
	if got := warning.String(); got != "browser launch failed; open the authorization URL manually\n" {
		t.Fatalf("warning = %q", got)
	}
	if strings.Contains(warning.String(), launchErr.Error()) {
		t.Fatalf("warning leaked launch detail: %q", warning.String())
	}
}

func TestBrowserAuthorizerListensBeforeFirstRegistration(t *testing.T) {
	var registrationCount atomic.Int32
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/clients":
			registrationCount.Add(1)
			var body struct {
				RedirectURIs []string `json:"redirect_uris"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.RedirectURIs) != 1 || !callbackIsListening(body.RedirectURIs[0]) {
				t.Fatalf("registration raced ahead of callback listener: %v", body.RedirectURIs)
			}
			_, _ = io.WriteString(w, `{"client_id":"fake-client","client_secret":"fake-secret"}`)
		case "/oauth2/token":
			writeFakeToken(w)
		default:
			http.NotFound(w, r)
		}
	})
	browser, browserResults := callbackBrowser(nil)
	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	authorizer.Random = bytes.NewReader(bytes.Repeat([]byte("A"), 64))
	authorizer.CallbackTimeout = time.Second
	reg, tokens, err := authorizer.Authorize(context.Background(), nil, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	assertBrowserResult(t, browserResults)
	if registrationCount.Load() != 1 || reg.ClientID != "fake-client" || tokens.AccessToken != "fake-access" {
		t.Fatalf("registrationCount=%d reg=%+v tokens=%+v", registrationCount.Load(), reg, tokens)
	}
}

func TestBrowserAuthorizerReusesAvailableSavedPortWithoutRegistration(t *testing.T) {
	redirectURI := freeRedirectURI(t)
	var registrationCount atomic.Int32
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/clients":
			registrationCount.Add(1)
			t.Fatal("saved callback port was available; registration must not be replaced")
		case "/oauth2/token":
			writeFakeToken(w)
		default:
			http.NotFound(w, r)
		}
	})
	browser, browserResults := callbackBrowser(nil)
	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	authorizer.Random = bytes.NewReader(bytes.Repeat([]byte("B"), 64))
	authorizer.CallbackTimeout = time.Second
	existing := &oauth.Registration{ClientID: "saved-client", ClientSecret: "saved-secret", RedirectURI: redirectURI}
	reg, _, err := authorizer.Authorize(context.Background(), existing, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	assertBrowserResult(t, browserResults)
	if registrationCount.Load() != 0 || reg != *existing {
		t.Fatalf("registrationCount=%d reg=%+v, want existing %+v", registrationCount.Load(), reg, *existing)
	}
}

func TestBrowserAuthorizerReplacesRegistrationWhenSavedPortUnavailable(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	savedURI := "http://" + occupied.Addr().String() + "/callback"
	var registeredURI string
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/clients":
			var body struct {
				RedirectURIs []string `json:"redirect_uris"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			registeredURI = body.RedirectURIs[0]
			_, _ = io.WriteString(w, `{"client_id":"replacement-client","client_secret":"replacement-secret"}`)
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.PostForm.Get("redirect_uri") != registeredURI {
				t.Fatalf("exchange redirect=%q, registered=%q", r.PostForm.Get("redirect_uri"), registeredURI)
			}
			writeFakeToken(w)
		default:
			http.NotFound(w, r)
		}
	})
	browser, browserResults := callbackBrowser(nil)
	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	authorizer.Random = bytes.NewReader(bytes.Repeat([]byte("C"), 64))
	authorizer.CallbackTimeout = time.Second
	existing := &oauth.Registration{ClientID: "saved-client", ClientSecret: "saved-secret", RedirectURI: savedURI}
	reg, _, err := authorizer.Authorize(context.Background(), existing, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	assertBrowserResult(t, browserResults)
	if registeredURI == "" || registeredURI == savedURI || reg.ClientID != "replacement-client" || reg.RedirectURI != registeredURI {
		t.Fatalf("registeredURI=%q savedURI=%q reg=%+v", registeredURI, savedURI, reg)
	}
}

func TestBrowserAuthorizerUsesFreshStateAndHandCheckedPKCES256(t *testing.T) {
	var tokenForm url.Values
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/clients":
			_, _ = io.WriteString(w, `{"client_id":"fake-client","client_secret":"fake-secret"}`)
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			tokenForm = cloneValues(r.PostForm)
			writeFakeToken(w)
		default:
			http.NotFound(w, r)
		}
	})
	var authQuery url.Values
	browser, browserResults := callbackBrowser(func(values url.Values) { authQuery = cloneValues(values) })
	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	random := append(bytes.Repeat([]byte("A"), 32), bytes.Repeat([]byte("B"), 32)...)
	authorizer.Random = bytes.NewReader(random)
	authorizer.CallbackTimeout = time.Second
	if _, _, err := authorizer.Authorize(context.Background(), nil, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertBrowserResult(t, browserResults)
	if authQuery.Get("state") != "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE" {
		t.Fatalf("state=%q", authQuery.Get("state"))
	}
	if authQuery.Get("code_challenge") != "dpSIltH1Qleurm3f_xNkUnBTdqwmVVafNnhuYkt-oNE" || authQuery.Get("code_challenge_method") != "S256" {
		t.Fatalf("PKCE query=%v", authQuery)
	}
	if tokenForm.Get("code_verifier") != "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI" {
		t.Fatalf("code_verifier=%q", tokenForm.Get("code_verifier"))
	}
}

func TestBrowserAuthorizerExchangesPastedCallbackWithoutLeakingCode(t *testing.T) {
	const pastedCode = "fake-pasted-code-never-print"
	var tokenCode string
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/clients":
			_, _ = io.WriteString(w, `{"client_id":"fake-client","client_secret":"fake-secret"}`)
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			tokenCode = r.PostForm.Get("code")
			writeFakeToken(w)
		default:
			http.NotFound(w, r)
		}
	})

	callbackInput, callbackWriter := io.Pipe()
	t.Cleanup(func() {
		_ = callbackInput.Close()
		_ = callbackWriter.Close()
	})
	pageResult := make(chan string, 1)
	browser := browserFunc(func(_ context.Context, rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		query := parsed.Query()
		callbackURL := query.Get("redirect_uri") + "?code=" + url.QueryEscape(pastedCode) + "&scope=offline+openid+all&state=" + url.QueryEscape(query.Get("state"))
		go func() {
			resp, requestErr := http.Get(callbackURL)
			if requestErr != nil {
				pageResult <- "ERROR"
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				pageResult <- "ERROR"
				return
			}
			pageResult <- string(body)
			_, _ = io.WriteString(callbackWriter, callbackURL+"\n")
		}()
		return nil
	})

	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	authorizer.Random = bytes.NewReader(bytes.Repeat([]byte("P"), 64))
	authorizer.CallbackTimeout = time.Second
	var notice bytes.Buffer
	_, tokens, err := authorizer.Authorize(context.Background(), nil, callbackInput, &notice)
	if err != nil {
		t.Fatal(err)
	}
	page := waitCallbackPage(t, pageResult)
	if tokens.AccessToken != "fake-access" || tokenCode != pastedCode {
		t.Fatalf("tokens=%+v token code=%q", tokens, tokenCode)
	}
	if !strings.Contains(page, "粘贴到终端") || strings.Contains(page, pastedCode) || strings.Contains(notice.String(), pastedCode) {
		t.Fatalf("page=%q notice=%q", page, notice.String())
	}
	if !strings.Contains(notice.String(), "Paste the complete callback URL") {
		t.Fatalf("manual prompt missing from notice=%q", notice.String())
	}
}

func TestBrowserAuthorizerPastedPKCEErrorUsesFreshSecondAttempt(t *testing.T) {
	var tokenVerifier string
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/clients":
			_, _ = io.WriteString(w, `{"client_id":"fake-client","client_secret":"fake-secret"}`)
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			tokenVerifier = r.PostForm.Get("code_verifier")
			writeFakeToken(w)
		default:
			http.NotFound(w, r)
		}
	})

	callbackInput, callbackWriter := io.Pipe()
	t.Cleanup(func() {
		_ = callbackInput.Close()
		_ = callbackWriter.Close()
	})
	var mu sync.Mutex
	var states []string
	browser := browserFunc(func(_ context.Context, rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		query := parsed.Query()
		state := query.Get("state")
		redirect := query.Get("redirect_uri")
		mu.Lock()
		attempt := len(states)
		states = append(states, state)
		mu.Unlock()
		go func() {
			if attempt == 0 {
				_, _ = io.WriteString(callbackWriter, redirect+"?error=invalid_request&error_description=PKCE+code+challenge+is+not+supported&state="+url.QueryEscape(state)+"\n")
				return
			}
			_, _ = io.WriteString(callbackWriter, redirect+"?code=fake-second-code&state="+url.QueryEscape(state)+"\n")
		}()
		return nil
	})

	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	random := append(bytes.Repeat([]byte("A"), 32), bytes.Repeat([]byte("B"), 32)...)
	random = append(random, bytes.Repeat([]byte("C"), 32)...)
	authorizer.Random = bytes.NewReader(random)
	authorizer.CallbackTimeout = time.Second
	if _, _, err := authorizer.Authorize(context.Background(), nil, callbackInput, io.Discard); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 2 || states[0] == states[1] || tokenVerifier != "" {
		t.Fatalf("states=%q token verifier=%q", states, tokenVerifier)
	}
}

func TestBrowserAuthorizerRetriesExplicitPKCERejectionOnceWithFreshState(t *testing.T) {
	var tokenCalls atomic.Int32
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/clients":
			_, _ = io.WriteString(w, `{"client_id":"fake-client","client_secret":"fake-secret"}`)
		case "/oauth2/token":
			call := tokenCalls.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if call == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"unsupported_code_challenge_method"}`)
				return
			}
			if r.PostForm.Get("code_verifier") != "" {
				t.Fatalf("fallback included code_verifier=%q", r.PostForm.Get("code_verifier"))
			}
			writeFakeToken(w)
		default:
			http.NotFound(w, r)
		}
	})
	var mu sync.Mutex
	var authQueries []url.Values
	browser, browserResults := callbackBrowser(func(values url.Values) {
		mu.Lock()
		authQueries = append(authQueries, cloneValues(values))
		mu.Unlock()
	})
	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	random := append(bytes.Repeat([]byte("A"), 32), bytes.Repeat([]byte("B"), 32)...)
	random = append(random, bytes.Repeat([]byte("C"), 32)...)
	authorizer.Random = bytes.NewReader(random)
	authorizer.CallbackTimeout = time.Second
	if _, _, err := authorizer.Authorize(context.Background(), nil, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertBrowserResult(t, browserResults)
	assertBrowserResult(t, browserResults)
	mu.Lock()
	defer mu.Unlock()
	if tokenCalls.Load() != 2 || len(authQueries) != 2 {
		t.Fatalf("tokenCalls=%d authorization attempts=%d", tokenCalls.Load(), len(authQueries))
	}
	if authQueries[0].Get("code_challenge") == "" || authQueries[1].Get("code_challenge") != "" || authQueries[0].Get("state") == authQueries[1].Get("state") {
		t.Fatalf("authorization queries=%v", authQueries)
	}
}

func TestBrowserAuthorizerRetriesExplicitPKCECallbackRejectionWithoutLeakingDescription(t *testing.T) {
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/clients":
			_, _ = io.WriteString(w, `{"client_id":"fake-client","client_secret":"fake-secret"}`)
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.PostForm.Get("code_verifier") != "" {
				t.Fatalf("callback fallback included code_verifier=%q", r.PostForm.Get("code_verifier"))
			}
			writeFakeToken(w)
		default:
			http.NotFound(w, r)
		}
	})

	const privateDescription = "code_challenge_method is not supported; never-leak-marker"
	var queries []url.Values
	callbackPages := make(chan string, 2)
	browser := browserFunc(func(_ context.Context, rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		query := parsed.Query()
		queries = append(queries, cloneValues(query))
		callbackURL := query.Get("redirect_uri")
		if len(queries) == 1 {
			callbackURL += "?error=invalid_request&error_description=" + url.QueryEscape(privateDescription) + "&state=" + url.QueryEscape(query.Get("state"))
		} else {
			callbackURL += "?code=fake-code&state=" + url.QueryEscape(query.Get("state"))
		}
		go func() {
			resp, requestErr := http.Get(callbackURL)
			if requestErr != nil {
				callbackPages <- "ERROR"
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				callbackPages <- "ERROR"
				return
			}
			callbackPages <- string(body)
		}()
		return nil
	})
	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	random := append(bytes.Repeat([]byte("A"), 32), bytes.Repeat([]byte("B"), 32)...)
	random = append(random, bytes.Repeat([]byte("C"), 32)...)
	authorizer.Random = bytes.NewReader(random)
	authorizer.CallbackTimeout = time.Second
	var notice bytes.Buffer
	_, tokens, err := authorizer.Authorize(context.Background(), nil, nil, &notice)
	firstPage := waitCallbackPage(t, callbackPages)
	if strings.Contains(firstPage, "never-leak-marker") || strings.Contains(notice.String(), "never-leak-marker") {
		t.Fatalf("private OAuth description leaked: page=%q notice=%q", firstPage, notice.String())
	}
	if err != nil {
		t.Fatalf("Authorize() error=%v, want one non-PKCE retry", err)
	}
	secondPage := waitCallbackPage(t, callbackPages)
	if strings.Contains(secondPage, "never-leak-marker") {
		t.Fatalf("private OAuth description leaked in success page: %q", secondPage)
	}
	if tokens.AccessToken != "fake-access" || len(queries) != 2 {
		t.Fatalf("tokens=%+v authorization attempts=%d", tokens, len(queries))
	}
	if queries[0].Get("code_challenge") == "" || queries[1].Get("code_challenge") != "" || queries[0].Get("state") == queries[1].Get("state") {
		t.Fatalf("authorization queries=%v", queries)
	}
}

func TestBrowserAuthorizerDoesNotFallbackOnAmbiguousTokenError(t *testing.T) {
	var tokenCalls atomic.Int32
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/clients":
			_, _ = io.WriteString(w, `{"client_id":"fake-client","client_secret":"fake-secret"}`)
		case "/oauth2/token":
			tokenCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_request","error_description":"ambiguous request failure"}`)
		default:
			http.NotFound(w, r)
		}
	})
	browser, browserResults := callbackBrowser(nil)
	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	authorizer.Random = bytes.NewReader(bytes.Repeat([]byte("D"), 64))
	authorizer.CallbackTimeout = time.Second
	if _, _, err := authorizer.Authorize(context.Background(), nil, nil, io.Discard); err == nil {
		t.Fatal("Authorize() error = nil, want ambiguous token error")
	}
	assertBrowserResult(t, browserResults)
	if tokenCalls.Load() != 1 {
		t.Fatalf("token calls=%d, want 1", tokenCalls.Load())
	}
}

func TestBrowserAuthorizerPrintsURLBeforeReturningBrowserLaunchFailure(t *testing.T) {
	launchErr := errors.New("fake browser launch failure")
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/clients" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"client_id":"fake-client","client_secret":"fake-secret"}`)
	})
	var openedURL string
	browser := browserFunc(func(_ context.Context, rawURL string) error { openedURL = rawURL; return launchErr })
	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	authorizer.Random = bytes.NewReader(bytes.Repeat([]byte("E"), 64))
	authorizer.CallbackTimeout = time.Second
	var notice bytes.Buffer
	_, _, err := authorizer.Authorize(context.Background(), nil, nil, &notice)
	if !errors.Is(err, launchErr) {
		t.Fatalf("Authorize() error=%v, want preserved launch cause", err)
	}
	if openedURL == "" || notice.String() != openedURL+"\n" {
		t.Fatalf("openedURL=%q notice=%q", openedURL, notice.String())
	}
}

func TestBrowserAuthorizerAppliesFiveMinuteDefaultTimeout(t *testing.T) {
	deadlineChecked := false
	launchErr := errors.New("stop after deadline inspection")
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/clients" {
			_, _ = io.WriteString(w, `{"client_id":"fake-client","client_secret":"fake-secret"}`)
			return
		}
		http.NotFound(w, r)
	})
	browser := browserFunc(func(ctx context.Context, _ string) error {
		deadline, ok := ctx.Deadline()
		remaining := time.Until(deadline)
		deadlineChecked = ok && remaining > 4*time.Minute+55*time.Second && remaining <= 5*time.Minute
		return launchErr
	})
	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	authorizer.Random = bytes.NewReader(bytes.Repeat([]byte("F"), 64))
	_, _, _ = authorizer.Authorize(context.Background(), nil, nil, io.Discard)
	if !deadlineChecked {
		t.Fatal("browser context did not carry the five-minute default deadline")
	}
}

type oauthFixture struct {
	server *httptest.Server
	client *oauth.Client
}

func newOAuthFixture(t *testing.T, handler http.HandlerFunc) *oauthFixture {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	hx := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1})
	return &oauthFixture{server: server, client: oauth.NewClient(server.URL, hx)}
}

func callbackBrowser(capture func(url.Values)) (oauth.Browser, <-chan error) {
	results := make(chan error, 4)
	return browserFunc(func(_ context.Context, rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		query := parsed.Query()
		if capture != nil {
			capture(query)
		}
		go func() {
			callbackURL := query.Get("redirect_uri") + "?code=fake-code&state=" + url.QueryEscape(query.Get("state"))
			resp, requestErr := http.Get(callbackURL)
			if requestErr == nil {
				resp.Body.Close()
			}
			results <- requestErr
		}()
		return nil
	}), results
}

func assertBrowserResult(t *testing.T, results <-chan error) {
	t.Helper()
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("browser callback request: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("browser callback request did not finish")
	}
}

func waitCallbackPage(t *testing.T, pages <-chan string) string {
	t.Helper()
	select {
	case page := <-pages:
		return page
	case <-time.After(time.Second):
		t.Fatal("browser callback page did not finish")
		return ""
	}
}

func callbackIsListening(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("tcp4", parsed.Host, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func freeRedirectURI(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return "http://" + address + "/callback"
}

func writeFakeToken(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"access_token":"fake-access","refresh_token":"fake-refresh","id_token":"fake-id","token_type":"Bearer","expires_in":3600}`)
}

func cloneValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for key, entries := range values {
		clone[key] = append([]string(nil), entries...)
	}
	return clone
}

func TestBrowserAuthorizerTimeoutClosesCallback(t *testing.T) {
	server := newOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/clients" {
			_, _ = io.WriteString(w, `{"client_id":"fake-client","client_secret":"fake-secret"}`)
			return
		}
		http.NotFound(w, r)
	})
	var openedRedirect string
	browser := browserFunc(func(_ context.Context, rawURL string) error {
		parsed, _ := url.Parse(rawURL)
		openedRedirect = parsed.Query().Get("redirect_uri")
		return nil
	})
	authorizer := oauth.NewBrowserAuthorizer(server.client, browser)
	authorizer.Random = bytes.NewReader(bytes.Repeat([]byte("G"), 64))
	authorizer.CallbackTimeout = 20 * time.Millisecond
	if _, _, err := authorizer.Authorize(context.Background(), nil, nil, io.Discard); err == nil {
		t.Fatal("Authorize() error = nil, want timeout")
	}
	if openedRedirect == "" || strings.Contains(openedRedirect, "0.0.0.0") {
		t.Fatalf("opened redirect=%q", openedRedirect)
	}
	if _, err := http.Get(openedRedirect + "?code=late&state=late"); err == nil {
		t.Fatal("authorizer left callback listener open after timeout")
	}
}
