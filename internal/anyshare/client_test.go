package anyshare_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/httpx"
)

type tokenSourceFake struct {
	mu    sync.Mutex
	force []bool
}

func (f *tokenSourceFake) Token(_ context.Context, forceRefresh bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.force = append(f.force, forceRefresh)
	if forceRefresh {
		return "new-access", nil
	}
	return "old-access", nil
}

func (f *tokenSourceFake) calls() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.force...)
}

type staticToken string

func (s staticToken) Token(context.Context, bool) (string, error) { return string(s), nil }

type errorToken struct{ err error }

func (s errorToken) Token(context.Context, bool) (string, error) { return "", s.err }

func newTestClient(server *httptest.Server, tokens interface {
	Token(context.Context, bool) (string, error)
}) *anyshare.Client {
	return anyshare.NewClient(server.URL, httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1}), tokens)
}

func TestClientRefreshesOnceAfter401(t *testing.T) {
	tokens := &tokenSourceFake{}
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/api/eacp/v1/user/get" || r.URL.RawQuery != "" {
			t.Fatalf("request = %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer old-access":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"expired fixture credential"}`)
		case "Bearer new-access":
			_, _ = io.WriteString(w, `{"userid":"u","username":"Fixture User","account":"fixture","type":"user"}`)
		default:
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		if strings.Contains(r.URL.String(), "access") {
			t.Fatalf("request URL leaked bearer token: %s", r.URL.String())
		}
	}))
	defer server.Close()

	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 1
	client := anyshare.NewClient(server.URL, httpx.New(transport, httpx.Policy{MaxAttempts: 1}), tokens)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	user, err := client.CurrentUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != "u" || user.Name != "Fixture User" || user.Account != "fixture" || user.Type != "user" {
		t.Fatalf("user = %#v", user)
	}
	if requests != 2 || !reflect.DeepEqual(tokens.calls(), []bool{false, true}) {
		t.Fatalf("requests=%d force=%v", requests, tokens.calls())
	}
}

func TestCurrentUserNormalizesOfficialName(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantName string
	}{
		{
			name:     "official field",
			response: `{"userid":"u","name":"Official Name","account":"fixture","type":"user"}`,
			wantName: "Official Name",
		},
		{
			name:     "official field wins over legacy field",
			response: `{"userid":"u","name":"Official Name","username":"Legacy Name","account":"fixture","type":"user"}`,
			wantName: "Official Name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertAuthorizedRequest(t, r, http.MethodPost, "/api/eacp/v1/user/get")
				_, _ = io.WriteString(w, tt.response)
			}))
			defer server.Close()

			user, err := newTestClient(server, staticToken("fixture-access")).CurrentUser(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if user.Name != tt.wantName {
				t.Fatalf("Name = %q, want %q", user.Name, tt.wantName)
			}
		})
	}
}

func TestClientRejectsInvalidServerBeforeTokenAccess(t *testing.T) {
	serverRequests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		serverRequests++
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	tests := []struct {
		name   string
		server string
		marker string
	}{
		{name: "plaintext", server: "http://" + host + "/prefix"},
		{name: "userinfo", server: "https://url-userinfo-secret@" + host + "/prefix", marker: "url-userinfo-secret"},
		{name: "missing host", server: "https:///url-host-secret", marker: "url-host-secret"},
		{name: "empty hostname", server: "https://:443/url-hostname-secret", marker: "url-hostname-secret"},
		{name: "query", server: server.URL + "/prefix?url-query-secret", marker: "url-query-secret"},
		{name: "force query", server: server.URL + "/prefix?"},
		{name: "fragment", server: server.URL + "/prefix#url-fragment-secret", marker: "url-fragment-secret"},
		{name: "opaque", server: "https:url-opaque-secret", marker: "url-opaque-secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := &tokenSourceFake{}
			client := anyshare.NewClient(tt.server, httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1}), tokens)
			_, err := client.CurrentUser(context.Background())
			if calls := tokens.calls(); len(calls) != 0 {
				t.Fatalf("token calls=%v, want none", calls)
			}
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Local {
				t.Fatalf("err=%v, want local category", err)
			}
			if strings.Contains(err.Error(), tt.server) || (tt.marker != "" && strings.Contains(err.Error(), tt.marker)) {
				t.Fatalf("error leaked rejected server input: %v", err)
			}
		})
	}
	if serverRequests != 0 {
		t.Fatalf("server requests=%d, want none", serverRequests)
	}
}

func TestClientPreservesValidServerPathPrefix(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodPost, "/tenant/prefix/api/eacp/v1/user/get")
		_, _ = io.WriteString(w, `{"userid":"u","name":"Fixture"}`)
	}))
	defer server.Close()

	client := anyshare.NewClient(server.URL+"/tenant/prefix/", httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1}), staticToken("fixture-access"))
	if _, err := client.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsOversizedSuccessResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodPost, "/api/eacp/v1/user/get")
		_, _ = io.WriteString(w, `{"account":"`+strings.Repeat("x", 9<<20)+`"}`)
	}))
	defer server.Close()

	_, err := newTestClient(server, staticToken("fixture-access")).CurrentUser(context.Background())
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
		t.Fatalf("err=%v, want remote category", err)
	}
}

func TestClientStopsAfterSecond401(t *testing.T) {
	tokens := &tokenSourceFake{}
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"expired fixture credential"}`)
	}))
	defer server.Close()

	_, err := newTestClient(server, tokens).CurrentUser(context.Background())
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Auth {
		t.Fatalf("err=%v, want auth category", err)
	}
	if requests != 2 || !reflect.DeepEqual(tokens.calls(), []bool{false, true}) {
		t.Fatalf("requests=%d force=%v", requests, tokens.calls())
	}
}

func TestClientMapsConflictWithoutLeakingResponseOrToken(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/api/efast/v1/dir/create" {
			t.Fatalf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":403016,"message":"duplicate name token-response-secret"}`)
	}))
	defer server.Close()

	_, err := newTestClient(server, staticToken("bearer-test-secret")).CreateDir(context.Background(), "gns://parent", "child")
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
		t.Fatalf("err=%v, want remote category", err)
	}
	if strings.Contains(err.Error(), "bearer-test-secret") || strings.Contains(err.Error(), "token-response-secret") {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestClientPreservesTokenErrorCategory(t *testing.T) {
	want := &apperr.Error{Category: apperr.Interrupted, Op: "token", Message: "canceled", Err: context.Canceled}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server contacted after token failure")
	}))
	defer server.Close()

	_, err := newTestClient(server, errorToken{err: want}).CurrentUser(context.Background())
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Interrupted || !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want preserved interrupted category and cause", err)
	}
}

func TestClientSanitizesTokenSourceErrors(t *testing.T) {
	categorizedCause := errors.New("token-cause-marker-secret")
	categorized := &apperr.Error{
		Category: apperr.Auth,
		Op:       "token-op-marker-secret",
		Message:  "token-message-marker-secret",
		Err:      categorizedCause,
	}
	plain := errors.New("token-plain-marker-secret")
	tests := []struct {
		name         string
		source       error
		wantCategory apperr.Category
		markers      []string
		unsafeCause  error
	}{
		{
			name:         "categorized",
			source:       categorized,
			wantCategory: apperr.Auth,
			markers:      []string{"token-op-marker-secret", "token-message-marker-secret", "token-cause-marker-secret"},
			unsafeCause:  categorizedCause,
		},
		{
			name:         "unclassified",
			source:       plain,
			wantCategory: apperr.Network,
			markers:      []string{"token-plain-marker-secret"},
			unsafeCause:  plain,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("server contacted after token failure")
			}))
			defer server.Close()

			_, err := newTestClient(server, errorToken{err: tt.source}).CurrentUser(context.Background())
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != tt.wantCategory || appErr.Op != "anyshare" || appErr.Message != "obtain access token" {
				t.Fatalf("err=%#v, want category=%s with safe AnyShare context", appErr, tt.wantCategory)
			}
			for _, marker := range tt.markers {
				if strings.Contains(err.Error(), marker) {
					t.Fatalf("error leaked %q: %v", marker, err)
				}
			}
			if errors.Is(err, tt.unsafeCause) {
				t.Fatalf("error retained unsafe source cause identity: %v", err)
			}
		})
	}
}

func TestClientSanitizesTokenCancellationAndPreservesSentinel(t *testing.T) {
	tests := []struct {
		name     string
		source   error
		sentinel error
		marker   string
	}{
		{name: "direct canceled", source: context.Canceled, sentinel: context.Canceled},
		{name: "wrapped canceled", source: fmt.Errorf("token-cancel-marker-secret: %w", context.Canceled), sentinel: context.Canceled, marker: "token-cancel-marker-secret"},
		{
			name: "categorized deadline",
			source: &apperr.Error{
				Category: apperr.Auth,
				Op:       "token-deadline-op-marker-secret",
				Message:  "token-deadline-message-marker-secret",
				Err:      context.DeadlineExceeded,
			},
			sentinel: context.DeadlineExceeded,
			marker:   "token-deadline",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("server contacted after token cancellation")
			}))
			defer server.Close()

			_, err := newTestClient(server, errorToken{err: tt.source}).CurrentUser(context.Background())
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Interrupted || !errors.Is(err, tt.sentinel) {
				t.Fatalf("err=%v, want interrupted category preserving %v", err, tt.sentinel)
			}
			if tt.marker != "" && strings.Contains(err.Error(), tt.marker) {
				t.Fatalf("error leaked cancellation marker: %v", err)
			}
		})
	}
}

func TestClientMapsInFlightCancellationToInterrupted(t *testing.T) {
	reached := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(reached)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := newTestClient(server, staticToken("fixture-access")).CurrentUser(ctx)
		result <- err
	}()
	<-reached
	cancel()
	err := <-result
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Interrupted {
		t.Fatalf("err=%v, want interrupted category", err)
	}
}
