package oauth_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/httpx"
	"github.com/Findddx/pku-drive-cli/internal/oauth"
)

func TestRegisterUsesLoopbackAndDeployedCompatibilityFlowSet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/clients" || r.Method != http.MethodPost {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"client_name":               "pku-drive-cli",
			"grant_types":               []any{"authorization_code", "refresh_token", "implicit"},
			"response_types":            []any{"token id_token", "code", "token"},
			"scope":                     "offline openid all",
			"redirect_uris":             []any{"http://127.0.0.1:12345/callback"},
			"post_logout_redirect_uris": []any{"http://127.0.0.1:12345/callback"},
			"metadata": map[string]any{"device": map[string]any{
				"name": "pku-drive-cli", "client_type": "web", "description": "PKU Drive CLI on Linux",
			}},
		}
		if !reflect.DeepEqual(body, want) {
			t.Fatalf("registration body=%#v, want %#v", body, want)
		}
		_, _ = io.WriteString(w, `{"client_id":"client","client_secret":"secret"}`)
	}))
	defer server.Close()

	hx := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1})
	got, err := oauth.NewClient(server.URL, hx).Register(context.Background(), "http://127.0.0.1:12345/callback")
	if err != nil || got != (oauth.Registration{ClientID: "client", ClientSecret: "secret", RedirectURI: "http://127.0.0.1:12345/callback"}) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestAuthorizationURLIncludesOAuthAndPKCEParameters(t *testing.T) {
	client := oauth.NewClient("https://disk.example", nil)
	reg := oauth.Registration{ClientID: "client id", RedirectURI: "http://127.0.0.1:12345/callback"}
	raw := client.AuthorizationURL(reg, "state value", "challenge/value")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Host != "disk.example" || parsed.Path != "/oauth2/auth" {
		t.Fatalf("authorization endpoint=%q", raw)
	}
	want := url.Values{
		"client_id":             {"client id"},
		"redirect_uri":          {"http://127.0.0.1:12345/callback"},
		"response_type":         {"code"},
		"scope":                 {"offline openid all"},
		"state":                 {"state value"},
		"code_challenge":        {"challenge/value"},
		"code_challenge_method": {"S256"},
	}
	if !reflect.DeepEqual(parsed.Query(), want) {
		t.Fatalf("query=%v, want %v", parsed.Query(), want)
	}
}

func TestExchangeUsesBasicAuthAndAuthorizationCodeForm(t *testing.T) {
	testTokenRequest(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"fake-code"},
		"redirect_uri":  {"http://127.0.0.1:12345/callback"},
		"code_verifier": {"fake-verifier"},
	}, func(client *oauth.Client, reg oauth.Registration) (oauth.TokenSet, error) {
		return client.Exchange(context.Background(), reg, "fake-code", "fake-verifier")
	})
}

func TestRefreshUsesBasicAuthAndRefreshTokenForm(t *testing.T) {
	testTokenRequest(t, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"fake-refresh"},
	}, func(client *oauth.Client, reg oauth.Registration) (oauth.TokenSet, error) {
		return client.Refresh(context.Background(), reg, "fake-refresh")
	})
}

func testTokenRequest(t *testing.T, wantForm url.Values, call func(*oauth.Client, oauth.Registration) (oauth.TokenSet, error)) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" || r.Method != http.MethodPost {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "fake-client" || password != "fake-secret" {
			t.Fatalf("basic auth ok=%v username=%q password=%q", ok, username, password)
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/x-www-form-urlencoded" {
			t.Fatalf("content-type=%q", contentType)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(r.PostForm, wantForm) {
			t.Fatalf("form=%v, want %v", r.PostForm, wantForm)
		}
		_, _ = io.WriteString(w, `{"access_token":"fake-access","refresh_token":"fake-new-refresh","id_token":"fake-id","token_type":"Bearer","expires_in":3600}`)
	}))
	defer server.Close()

	hx := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1})
	reg := oauth.Registration{ClientID: "fake-client", ClientSecret: "fake-secret", RedirectURI: "http://127.0.0.1:12345/callback"}
	got, err := call(oauth.NewClient(server.URL, hx), reg)
	want := oauth.TokenSet{AccessToken: "fake-access", RefreshToken: "fake-new-refresh", IDToken: "fake-id", TokenType: "Bearer", ExpiresIn: time.Hour}
	if err != nil || got != want {
		t.Fatalf("got=%+v err=%v, want %+v", got, err, want)
	}
}

func TestRevokePostsExactlyOneTokenWithBasicAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/revoke" || r.Method != http.MethodPost {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "fake-client" || password != "fake-secret" {
			t.Fatalf("basic auth ok=%v username=%q password=%q", ok, username, password)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(r.PostForm, url.Values{"token": {"fake-token"}}) {
			t.Fatalf("form=%v", r.PostForm)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	hx := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1})
	reg := oauth.Registration{ClientID: "fake-client", ClientSecret: "fake-secret"}
	if err := oauth.NewClient(server.URL, hx).Revoke(context.Background(), reg, "fake-token"); err != nil {
		t.Fatal(err)
	}
}

func TestExchangeRejectsExpiryThatCannotFitDuration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"fake-access","expires_in":9223372037}`)
	}))
	defer server.Close()
	hx := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1})
	reg := oauth.Registration{ClientID: "fake-client", ClientSecret: "fake-secret", RedirectURI: "http://127.0.0.1:12345/callback"}
	if _, err := oauth.NewClient(server.URL, hx).Exchange(context.Background(), reg, "fake-code", "fake-verifier"); err == nil {
		t.Fatal("Exchange() error = nil, want expiry overflow rejection")
	}
}
