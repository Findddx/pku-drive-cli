package httpx

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

const (
	defaultAttempts = 5
	defaultDelay    = 100 * time.Millisecond
	maxDrain        = 64 << 10
	maxDuration     = time.Duration(1<<63 - 1)
)

var errNilRequest = errors.New("nil request")

func requestError(message string, err error) error {
	if err == nil {
		return nil
	}
	return apperr.Wrap(apperr.Network, "http", message, safeTransportError(err))
}

func safeTransportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return errors.New(urlErr.Op + " " + privateRedactURL(urlErr.URL))
	}
	return errors.New("transport error")
}

func privateRedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "request URL redacted"
	}
	return RedactURL(parsed)
}

func isSecretQueryKey(key string) bool {
	key = strings.ToLower(key)
	compact := strings.NewReplacer("-", "", "_", "").Replace(key)
	return compact == "code" || compact == "state" || compact == "codeverifier" || compact == "errordescription" || strings.Contains(compact, "token") || strings.Contains(compact, "signature") || strings.HasPrefix(key, "x-amz-") || strings.Contains(compact, "accesskey") || strings.Contains(compact, "credential")
}

func (c *Client) Do(ctx context.Context, req *http.Request, idempotent bool) (*http.Response, error) {
	if req == nil {
		return nil, requestError("http request failed", errNilRequest)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	attempts := c.Policy.MaxAttempts
	if attempts == 0 {
		attempts = defaultAttempts
	}
	canRetry := idempotent && (req.Body == nil || req.GetBody != nil)

	for attempt := 0; attempt < attempts; attempt++ {
		attemptReq, err := cloneRequest(ctx, req, attempt)
		if err != nil {
			return nil, requestError("http request failed", err)
		}
		resp, err := c.HTTP.Do(attemptReq)
		if err == nil && !temporaryStatus(resp.StatusCode) {
			return resp, nil
		}
		if !canRetry || attempt+1 == attempts || (err != nil && !temporaryTransportError(err)) {
			if err != nil {
				return nil, requestError("http request failed", err)
			}
			return resp, nil
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
			_ = resp.Body.Close()
		}
		if err := c.sleep(ctx, retryDelay(c.Policy, resp, attempt)); err != nil {
			return nil, requestError("http request failed", err)
		}
	}
	panic("unreachable")
}

func temporaryTransportError(err error) bool {
	var networkErr net.Error
	if !errors.As(err, &networkErr) {
		return false
	}
	if networkErr.Timeout() {
		return true
	}
	temporary, ok := networkErr.(interface{ Temporary() bool })
	return ok && temporary.Temporary()
}

func cloneRequest(ctx context.Context, req *http.Request, attempt int) (*http.Request, error) {
	clone := req.Clone(ctx)
	if attempt > 0 && req.Body != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		clone.Body = body
	}
	return clone, nil
}

func temporaryStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func retryDelay(policy Policy, resp *http.Response, attempt int) time.Duration {
	if retryAfter, ok := parseRetryAfter(resp); ok {
		return capDelay(retryAfter, policy.MaxDelay)
	}
	delay := policy.BaseDelay
	if delay <= 0 {
		delay = defaultDelay
	}
	for i := 0; i < attempt; i++ {
		delay *= 2
	}
	delay = capDelay(delay, policy.MaxDelay)
	if delay > 1 {
		delay = delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))
	}
	return delay
}

func capDelay(delay, max time.Duration) time.Duration {
	if max > 0 && delay > max {
		return max
	}
	return delay
}

func parseRetryAfter(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds > int64(maxDuration/time.Second) {
			return maxDuration, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay, true
		}
	}
	return 0, false
}

func (c *Client) sleep(ctx context.Context, delay time.Duration) error {
	if c.Sleep == nil {
		return sleepContext(ctx, delay)
	}
	return c.Sleep(ctx, delay)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
