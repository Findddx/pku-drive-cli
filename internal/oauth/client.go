// Package oauth implements the browser-mediated OAuth protocol used by PKU Drive.
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/httpx"
)

const maxExpirySeconds = int64((1<<63 - 1) / int64(time.Second))

var errPKCEUnsupported = errors.New("PKCE is unsupported")

type Registration struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

type TokenSet struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	ExpiresIn    time.Duration
}

type Client struct {
	server string
	http   *httpx.Client
}

type registrationResponse struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

func NewClient(server string, httpClient *httpx.Client) *Client {
	return &Client{server: strings.TrimRight(server, "/"), http: httpClient}
}

func (c *Client) Register(ctx context.Context, redirectURI string) (Registration, error) {
	body := struct {
		ClientName             string         `json:"client_name"`
		GrantTypes             []string       `json:"grant_types"`
		ResponseTypes          []string       `json:"response_types"`
		Scope                  string         `json:"scope"`
		RedirectURIs           []string       `json:"redirect_uris"`
		PostLogoutRedirectURIs []string       `json:"post_logout_redirect_uris"`
		Metadata               map[string]any `json:"metadata"`
	}{
		ClientName: "pku-drive-cli",
		// The deployed AnyShare 7.0.6.3 registration schema requires at
		// least three advertised grant and response types. The CLI itself
		// still executes only authorization_code (with PKCE) and refresh_token.
		GrantTypes:             []string{"authorization_code", "refresh_token", "implicit"},
		ResponseTypes:          []string{"token id_token", "code", "token"},
		Scope:                  "offline openid all",
		RedirectURIs:           []string{redirectURI},
		PostLogoutRedirectURIs: []string{redirectURI},
		Metadata: map[string]any{"device": map[string]string{
			"name": "pku-drive-cli", "client_type": "web", "description": "PKU Drive CLI on Linux",
		}},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return Registration{}, authError("register", "encode registration", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/oauth2/clients"), bytes.NewReader(encoded))
	if err != nil {
		return Registration{}, authError("register", "create request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(ctx, req)
	if err != nil {
		return Registration{}, authError("register", "request failed", err)
	}
	if err := requireSuccess(resp); err != nil {
		return Registration{}, authError("register", "request rejected", err)
	}
	var wire registrationResponse
	if err := httpx.DecodeJSON(resp, httpx.DefaultMaxResponse, &wire); err != nil {
		return Registration{}, authError("register", "invalid response", err)
	}
	return Registration{ClientID: wire.ClientID, ClientSecret: wire.ClientSecret, RedirectURI: redirectURI}, nil
}

func (c *Client) AuthorizationURL(reg Registration, state, codeChallenge string) string {
	values := url.Values{
		"client_id":     {reg.ClientID},
		"redirect_uri":  {reg.RedirectURI},
		"response_type": {"code"},
		"scope":         {"offline openid all"},
		"state":         {state},
	}
	if codeChallenge != "" {
		values.Set("code_challenge", codeChallenge)
		values.Set("code_challenge_method", "S256")
	}
	return c.endpoint("/oauth2/auth") + "?" + values.Encode()
}

func (c *Client) Exchange(ctx context.Context, reg Registration, code, verifier string) (TokenSet, error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {reg.RedirectURI},
	}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	return c.token(ctx, reg, form, "exchange")
}

func (c *Client) Refresh(ctx context.Context, reg Registration, refreshToken string) (TokenSet, error) {
	return c.token(ctx, reg, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}, "refresh")
}

func (c *Client) token(ctx context.Context, reg Registration, form url.Values, op string) (TokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/oauth2/token"), strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, authError(op, "create request", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(reg.ClientID, reg.ClientSecret)
	resp, err := c.do(ctx, req)
	if err != nil {
		return TokenSet{}, authError(op, "request failed", err)
	}
	if err := tokenResponseStatus(resp); err != nil {
		return TokenSet{}, authError(op, "request rejected", err)
	}
	var wire tokenResponse
	if err := httpx.DecodeJSON(resp, httpx.DefaultMaxResponse, &wire); err != nil {
		return TokenSet{}, authError(op, "invalid response", err)
	}
	if wire.ExpiresIn < 0 || wire.ExpiresIn > maxExpirySeconds {
		return TokenSet{}, authError(op, "invalid response", errors.New("token expiry is out of range"))
	}
	return TokenSet{
		AccessToken: wire.AccessToken, RefreshToken: wire.RefreshToken, IDToken: wire.IDToken,
		TokenType: wire.TokenType, ExpiresIn: time.Duration(wire.ExpiresIn) * time.Second,
	}, nil
}

func tokenResponseStatus(resp *http.Response) error {
	if resp == nil {
		return errors.New("empty HTTP response")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var wire struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(data, &wire) == nil && explicitUnsupportedPKCE(wire.Error, wire.Description) {
		return errPKCEUnsupported
	}
	return fmt.Errorf("HTTP status %d", resp.StatusCode)
}

func explicitUnsupportedPKCE(code, description string) bool {
	if code == "unsupported_code_challenge_method" {
		return true
	}
	if code != "invalid_request" {
		return false
	}
	normalized := strings.ToLower(description)
	mentionsPKCE := strings.Contains(normalized, "pkce") || strings.Contains(normalized, "code_challenge") || strings.Contains(normalized, "code challenge")
	unsupported := strings.Contains(normalized, "not supported") || strings.Contains(normalized, "unsupported")
	return mentionsPKCE && unsupported
}

func (c *Client) Revoke(ctx context.Context, reg Registration, token string) error {
	form := url.Values{"token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/oauth2/revoke"), strings.NewReader(form.Encode()))
	if err != nil {
		return authError("revoke", "create request", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(reg.ClientID, reg.ClientSecret)
	resp, err := c.do(ctx, req)
	if err != nil {
		return authError("revoke", "request failed", err)
	}
	if err := requireSuccess(resp); err != nil {
		return authError("revoke", "request rejected", err)
	}
	_ = resp.Body.Close()
	return nil
}

func (c *Client) endpoint(path string) string {
	return c.server + path
}

func (c *Client) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("OAuth HTTP client is missing")
	}
	return c.http.Do(ctx, req, false)
}

func requireSuccess(resp *http.Response) error {
	if resp == nil {
		return errors.New("empty HTTP response")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}
	return fmt.Errorf("HTTP status %d", resp.StatusCode)
}

func authError(op, message string, err error) error {
	return apperr.Wrap(apperr.Auth, "oauth "+op, message, err)
}
