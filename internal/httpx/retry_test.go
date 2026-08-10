package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

type errorTransport struct {
	attempts int
	err      error
}

func (t *errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.attempts++
	return nil, t.err
}

func TestDoDoesNotRetryPermanentTransportErrors(t *testing.T) {
	transport := &errorTransport{err: errors.New("certificate rejected")}
	client := New(transport, Policy{MaxAttempts: 3, BaseDelay: time.Millisecond})
	client.Sleep = func(context.Context, time.Duration) error { return nil }
	req, err := http.NewRequest(http.MethodGet, "https://drive.example/files", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(context.Background(), req, true); err == nil {
		t.Fatal("Do() error = nil, want transport error")
	}
	if transport.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", transport.attempts)
	}
}

func TestDoRedactsSignedQueryInTransportError(t *testing.T) {
	transport := &errorTransport{err: errors.New("connection refused")}
	client := New(transport, Policy{MaxAttempts: 1})
	req, err := http.NewRequest(http.MethodGet, "https://drive.example/download?X-Amz-Signature=signature-secret&part=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(context.Background(), req, true)
	if err == nil {
		t.Fatal("Do() error = nil, want transport error")
	}
	if got := err.Error(); containsAny(got, "signature-secret", "Authorization", "Cookie") {
		t.Fatalf("Do() leaked a secret in %q", got)
	}
}

func TestDoReturnsNetworkCategoryForTransportFailure(t *testing.T) {
	client := New(&errorTransport{err: errors.New("connection refused")}, Policy{MaxAttempts: 1})
	req, err := http.NewRequest(http.MethodGet, "https://drive.example/files", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(context.Background(), req, true)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Network {
		t.Fatalf("Do() error = %#v, want apperr.Network", err)
	}
}

func containsAny(s string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(s, value) {
			return true
		}
	}
	return false
}

func TestDoReplaysPOSTBodyFromGetBody(t *testing.T) {
	var bodies []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(body))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.Client().Transport, Policy{MaxAttempts: 2, BaseDelay: time.Millisecond})
	client.Sleep = func(context.Context, time.Duration) error { return nil }
	req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("byte-exact"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(context.Background(), req, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got, want := strings.Join(bodies, ","), "byte-exact,byte-exact"; got != want {
		t.Fatalf("received bodies = %q, want %q", got, want)
	}
}

func TestDoDoesNotRetryNonReplayableBody(t *testing.T) {
	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := New(server.Client().Transport, Policy{MaxAttempts: 3, BaseDelay: time.Millisecond})
	client.Sleep = func(context.Context, time.Duration) error { return nil }
	req, err := http.NewRequest(http.MethodPost, server.URL, io.NopCloser(strings.NewReader("one-shot")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(context.Background(), req, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

type closeTrackedBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackedBody) Close() error {
	b.closed = true
	return nil
}

type responseTransport struct {
	attempts int
	first    *closeTrackedBody
}

func (t *responseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.attempts++
	if t.attempts == 1 {
		t.first = &closeTrackedBody{Reader: strings.NewReader("retry")}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: t.first}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func TestDoClosesTemporaryResponseBeforeRetry(t *testing.T) {
	transport := &responseTransport{}
	client := New(transport, Policy{MaxAttempts: 2, BaseDelay: time.Millisecond})
	client.Sleep = func(context.Context, time.Duration) error { return nil }
	req, err := http.NewRequest(http.MethodGet, "https://drive.example/files", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(context.Background(), req, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if transport.first == nil || !transport.first.closed {
		t.Fatal("temporary response body was not closed")
	}
}

func TestDoUsesFiveAttemptsByDefault(t *testing.T) {
	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer server.Close()
	client := New(server.Client().Transport, Policy{})
	client.Sleep = func(context.Context, time.Duration) error { return nil }
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(context.Background(), req, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if attempts != 5 {
		t.Fatalf("attempts = %d, want 5", attempts)
	}
}

func TestDoHonorsRetryAfterSeconds(t *testing.T) {
	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.Client().Transport, Policy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Second})
	var slept time.Duration
	client.Sleep = func(ctx context.Context, delay time.Duration) error {
		slept = delay
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(context.Background(), req, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if slept != 2*time.Second {
		t.Fatalf("retry delay = %s, want 2s", slept)
	}
}

func TestDoHonorsRetryAfterHTTPDate(t *testing.T) {
	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", time.Now().Add(3*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.Client().Transport, Policy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Second})
	var slept time.Duration
	client.Sleep = func(ctx context.Context, delay time.Duration) error {
		slept = delay
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(context.Background(), req, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if slept <= 0 || slept > 3*time.Second {
		t.Fatalf("retry delay = %s, want a positive HTTP-date delay no greater than 3s", slept)
	}
}

func TestDoCapsOverflowingRetryAfterSeconds(t *testing.T) {
	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "9223372036854775807")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.Client().Transport, Policy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Second})
	var slept time.Duration
	client.Sleep = func(ctx context.Context, delay time.Duration) error {
		slept = delay
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(context.Background(), req, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if slept != 2*time.Second {
		t.Fatalf("retry delay = %s, want capped 2s", slept)
	}
}
