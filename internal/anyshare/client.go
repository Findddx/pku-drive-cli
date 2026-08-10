// Package anyshare implements the authenticated AnyShare document API.
package anyshare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/auth"
	"github.com/Findddx/pku-drive-cli/internal/httpx"
)

const maxResponseBytes int64 = 8 << 20

var (
	errInvalidServer = errors.New("invalid server URL")
	errTokenSource   = errors.New("token source failed")
)

type responseStatusError struct{ StatusCode int }

func (e *responseStatusError) Error() string { return http.StatusText(e.StatusCode) }

// ErrRequestNotSent marks a failure that occurred before an HTTP request was
// dispatched. It is safe for a caller to retry the intended operation.
var ErrRequestNotSent = errors.New("request was not sent")

// IsRequestNotSent reports whether the remote operation could not have been
// committed because its HTTP request was never dispatched.
func IsRequestNotSent(err error) bool { return errors.Is(err, ErrRequestNotSent) }

type requestNotSentError struct{ err error }

func (e *requestNotSentError) Error() string {
	if e == nil || e.err == nil {
		return ErrRequestNotSent.Error()
	}
	return e.err.Error()
}

func (e *requestNotSentError) Unwrap() []error {
	if e == nil || e.err == nil {
		return []error{ErrRequestNotSent}
	}
	return []error{ErrRequestNotSent, e.err}
}

func markRequestNotSent(err error) error {
	if err == nil || IsRequestNotSent(err) {
		return err
	}
	return &requestNotSentError{err: err}
}

// User describes the currently authenticated AnyShare user.
type User struct {
	ID      string `json:"userid"`
	Name    string `json:"username"`
	Account string `json:"account"`
	Type    string `json:"type"`
}

type userWire struct {
	ID       string `json:"userid"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Account  string `json:"account"`
	Type     string `json:"type"`
}

// Client is an authenticated AnyShare API client.
type Client struct {
	server     string
	httpClient *httpx.Client
	tokens     auth.TokenSource
	initErr    error
}

// NewClient constructs an AnyShare API client.
func NewClient(server string, httpClient *httpx.Client, tokens auth.TokenSource) *Client {
	normalizedServer, err := normalizeServer(server)
	return &Client{server: normalizedServer, httpClient: httpClient, tokens: tokens, initErr: err}
}

func normalizeServer(server string) (string, error) {
	parsed, err := url.Parse(server)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", errInvalidServer
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// CurrentUser returns the authenticated user's profile.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var wireUser userWire
	if err := c.callJSON(ctx, http.MethodPost, "/api/eacp/v1/user/get", nil, true, &wireUser); err != nil {
		return User{}, err
	}
	name := wireUser.Name
	if name == "" {
		name = wireUser.Username
	}
	return User{ID: wireUser.ID, Name: name, Account: wireUser.Account, Type: wireUser.Type}, nil
}

func (c *Client) callJSON(ctx context.Context, method, endpoint string, body []byte, idempotent bool, dst any) error {
	resp, err := c.do(ctx, method, endpoint, body, idempotent)
	if err != nil {
		return err
	}
	if err := httpx.DecodeJSON(resp, maxResponseBytes, dst); err != nil {
		return wrapError(apperr.Remote, "anyshare", "invalid API response", err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, endpoint string, body []byte, idempotent bool) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		return nil, wrapError(apperr.Local, "anyshare", "client is not configured", errors.New("missing client dependency"))
	}
	if c.initErr != nil {
		return nil, wrapError(apperr.Local, "anyshare", "client is not configured", c.initErr)
	}
	if c.httpClient == nil || c.tokens == nil || c.server == "" {
		return nil, wrapError(apperr.Local, "anyshare", "client is not configured", errors.New("missing client dependency"))
	}

	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.tokens.Token(ctx, attempt == 1)
		if err != nil {
			return nil, markRequestNotSent(sanitizeTokenError(err))
		}
		if token == "" {
			return nil, markRequestNotSent(wrapError(apperr.Auth, "anyshare", "login required", errors.New("access token is empty")))
		}
		req, err := http.NewRequestWithContext(ctx, method, c.server+endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, wrapError(apperr.Local, "anyshare", "build request", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(ctx, req, idempotent)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, wrapError(apperr.Interrupted, "anyshare", "request interrupted", contextErr)
			}
			return nil, preserveError("anyshare", "request failed", err)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			discardAndClose(resp.Body)
			if attempt == 0 {
				continue
			}
			return nil, wrapError(apperr.Auth, "anyshare", "login required", errors.New("server rejected refreshed credentials"))
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			discardAndClose(resp.Body)
			return nil, wrapError(apperr.Remote, "anyshare", "API request failed", &responseStatusError{StatusCode: resp.StatusCode})
		}
		return resp, nil
	}
	panic("unreachable")
}

func sanitizeTokenError(err error) error {
	if errors.Is(err, context.Canceled) {
		return wrapError(apperr.Interrupted, "anyshare", "obtain access token", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return wrapError(apperr.Interrupted, "anyshare", "obtain access token", context.DeadlineExceeded)
	}
	category := apperr.Network
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		category = appErr.Category
	}
	return wrapError(category, "anyshare", "obtain access token", errTokenSource)
}

func discardAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes+1))
	_ = body.Close()
}

func preserveError(op, message string, err error) error {
	if err == nil {
		return nil
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return wrapError(appErr.Category, op, message, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return wrapError(apperr.Interrupted, op, message, err)
	}
	return wrapError(apperr.Network, op, message, err)
}

func wrapError(category apperr.Category, op, message string, err error) error {
	return apperr.Wrap(category, op, message, err)
}

func marshalBody(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, wrapError(apperr.Local, "anyshare", "encode request", err)
	}
	return body, nil
}
