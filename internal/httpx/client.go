// Package httpx provides the restricted HTTP boundary used by remote clients.
package httpx

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"
)

const DefaultMaxResponse int64 = 8 << 20

// Policy controls bounded request retries.
type Policy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// Client is an HTTP client with strict transport and retry policy.
type Client struct {
	HTTP   *http.Client
	Policy Policy
	Sleep  func(context.Context, time.Duration) error
}

// New creates a strict client based on base.
func New(base http.RoundTripper, policy Policy) *Client {
	transport := strictTransport(base)
	return &Client{
		HTTP: &http.Client{
			Transport:     transport,
			CheckRedirect: rejectUnsafeRedirect,
		},
		Policy: policy,
		Sleep:  sleepContext,
	}
}

func rejectUnsafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 || req.URL.Scheme != "https" || req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return errors.New("redirect rejected")
	}
	return nil
}

func strictTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	transport, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	clone := transport.Clone()
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &tls.Config{}
	} else {
		clone.TLSClientConfig = clone.TLSClientConfig.Clone()
	}
	clone.TLSClientConfig.MinVersion = tls.VersionTLS12
	clone.TLSClientConfig.InsecureSkipVerify = false
	if clone.DialContext == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		clone.DialContext = dialer.DialContext
	}
	if clone.TLSHandshakeTimeout == 0 {
		clone.TLSHandshakeTimeout = 10 * time.Second
	}
	if clone.ResponseHeaderTimeout == 0 {
		clone.ResponseHeaderTimeout = 15 * time.Second
	}
	return clone
}
