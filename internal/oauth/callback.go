package oauth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type callbackResult struct {
	code string
	err  error
}

type Callback struct {
	listener    net.Listener
	server      *http.Server
	redirectURI string
	result      chan callbackResult
	done        chan struct{}
	stateReady  chan struct{}
	state       string
	waitStarted atomic.Bool
	manual      atomic.Bool
	terminal    atomic.Bool
	closeOnce   sync.Once
	closeErr    error
}

func ListenCallback(redirectURI string) (*Callback, error) {
	address := "127.0.0.1:0"
	if redirectURI != "" {
		validated, err := validateRedirectURI(redirectURI)
		if err != nil {
			return nil, authError("callback", "invalid redirect URI", err)
		}
		address = validated
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, authError("callback", "listen failed", err)
	}
	if redirectURI == "" {
		redirectURI = "http://" + listener.Addr().String() + "/callback"
	}
	callback := &Callback{
		listener: listener, redirectURI: redirectURI, result: make(chan callbackResult, 1),
		done: make(chan struct{}), stateReady: make(chan struct{}),
	}
	callback.server = &http.Server{Handler: http.HandlerFunc(callback.handle)}
	go func() {
		_ = callback.server.Serve(listener)
	}()
	return callback, nil
}

func validateRedirectURI(raw string) (string, error) {
	if strings.Contains(raw, "#") {
		return "", errors.New("redirect URI must not contain a fragment")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("redirect URI is malformed")
	}
	if parsed.Scheme != "http" || parsed.Opaque != "" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.Path != "/callback" || parsed.EscapedPath() != "/callback" {
		return "", errors.New("redirect URI must be an exact IPv4 loopback callback")
	}
	portText := parsed.Port()
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || parsed.Host != net.JoinHostPort("127.0.0.1", portText) {
		return "", errors.New("redirect URI must contain a nonzero numeric port")
	}
	return parsed.Host, nil
}

func (c *Callback) RedirectURI() string {
	if c == nil {
		return ""
	}
	return c.redirectURI
}

func (c *Callback) Wait(ctx context.Context, state string) (string, error) {
	if c == nil {
		return "", authError("callback", "wait failed", errors.New("callback is missing"))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !c.waitStarted.CompareAndSwap(false, true) {
		return "", authError("callback", "wait failed", errors.New("callback wait already started"))
	}
	c.state = state
	close(c.stateReady)

	select {
	case result := <-c.result:
		return result.code, result.err
	case <-ctx.Done():
		_ = c.Close()
		return "", authError("callback", "wait canceled", ctx.Err())
	case <-c.done:
		return "", authError("callback", "wait failed", errors.New("callback closed"))
	}
}

// BeginManual prepares the loopback listener for a callback that will be
// copied from a browser and pasted into the terminal. HTTP requests are still
// answered, but only the pasted URL can complete the authorization attempt.
func (c *Callback) BeginManual(state string) error {
	if c == nil {
		return authError("callback", "manual callback failed", errors.New("callback is missing"))
	}
	if state == "" {
		return authError("callback", "manual callback failed", errors.New("callback state is missing"))
	}
	if !c.waitStarted.CompareAndSwap(false, true) {
		return authError("callback", "manual callback failed", errors.New("callback wait already started"))
	}
	c.state = state
	c.manual.Store(true)
	close(c.stateReady)
	return nil
}

func (c *Callback) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		close(c.done)
		listenerErr := c.listener.Close()
		serverErr := c.server.Close()
		if listenerErr != nil && !errors.Is(listenerErr, net.ErrClosed) {
			c.closeErr = listenerErr
		} else if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) && !errors.Is(serverErr, net.ErrClosed) {
			c.closeErr = serverErr
		}
	})
	return c.closeErr
}

func (c *Callback) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/callback" {
		writeCallbackPage(w, http.StatusNotFound, "授权失败：回调地址无效")
		return
	}
	query := r.URL.Query()
	requestState := query.Get("state")
	if requestState == "" {
		writeCallbackPage(w, http.StatusBadRequest, "授权失败：缺少状态参数")
		return
	}
	select {
	case <-c.stateReady:
	case <-c.done:
		writeCallbackPage(w, http.StatusGone, "授权失败：回调已关闭")
		return
	}
	if requestState != c.state {
		writeCallbackPage(w, http.StatusBadRequest, "授权失败：状态校验失败")
		return
	}
	if c.manual.Load() {
		writeCallbackPage(w, http.StatusOK, "请复制浏览器地址栏中的完整回调地址并粘贴到终端")
		return
	}

	var result callbackResult
	status := http.StatusOK
	message := "授权成功，可以关闭此页面"
	if oauthError := query.Get("error"); oauthError != "" {
		if explicitUnsupportedPKCE(oauthError, query.Get("error_description")) {
			result.err = authError("authorize", "authorization server rejected PKCE", errPKCEUnsupported)
		} else {
			result.err = authError("authorize", "authorization server returned an error", errors.New("authorization denied"))
		}
		status = http.StatusBadRequest
		message = "授权失败，请返回终端重试"
	} else if code := query.Get("code"); code != "" {
		result.code = code
	} else {
		writeCallbackPage(w, http.StatusBadRequest, "授权失败：缺少授权码")
		return
	}

	if !c.terminal.CompareAndSwap(false, true) {
		writeCallbackPage(w, http.StatusConflict, "授权失败：回调已处理")
		return
	}
	writeCallbackPage(w, status, message)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	c.result <- result
	_ = c.listener.Close()
}

func writeCallbackPage(w http.ResponseWriter, status int, message string) {
	body := []byte("<!doctype html><html lang=\"zh-CN\"><meta charset=\"utf-8\"><title>PKU Drive CLI</title><p>" + message + "</p></html>")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
