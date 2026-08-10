package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoRetriesTemporaryStatus(t *testing.T) {
	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	client := New(server.Client().Transport, Policy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(context.Background(), req, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || attempts != 3 {
		t.Fatalf("status=%d attempts=%d, want status=200 attempts=3", resp.StatusCode, attempts)
	}
}

func TestAuthorizationNeverCrossesHostRedirect(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Store(true)
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	client := New(source.Client().Transport, Policy{MaxAttempts: 1})
	req, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer never-forward")
	if _, err := client.Do(context.Background(), req, true); err == nil {
		t.Fatal("Do() error = nil, want redirect rejection")
	}
	if reached.Load() {
		t.Fatal("redirect target received a request")
	}
}

func TestDoRejectsHTTPSDowngradeBeforeTargetContact(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Store(true)
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	client := New(source.Client().Transport, Policy{MaxAttempts: 1})
	req, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(context.Background(), req, true); err == nil {
		t.Fatal("Do() error = nil, want HTTPS downgrade rejection")
	}
	if reached.Load() {
		t.Fatal("downgrade target received a request")
	}
}

func TestDoRejectsSameOriginRedirectLoopAtDefaultCeiling(t *testing.T) {
	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts <= 10 {
			http.Redirect(w, r, "/loop", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.Client().Transport, Policy{MaxAttempts: 1})
	req, err := http.NewRequest(http.MethodGet, server.URL+"/loop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(context.Background(), req, true); err == nil {
		t.Fatal("Do() error = nil, want redirect-loop rejection")
	}
	if attempts != 10 {
		t.Fatalf("requests = %d, want 10 before default redirect ceiling", attempts)
	}
}
