package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

const defaultCallbackTimeout = 5 * time.Minute

type Browser interface {
	Open(ctx context.Context, rawURL string) error
}

type BrowserAuthorizer struct {
	OAuth           *Client
	Browser         Browser
	Random          io.Reader
	CallbackTimeout time.Duration
}

type SystemBrowser struct {
	LookupEnv func(string) (string, bool)
	Command   func(context.Context, string, ...string) error
	Warning   io.Writer
}

type hiddenCause struct {
	message string
	err     error
}

func (e *hiddenCause) Error() string { return e.message }
func (e *hiddenCause) Unwrap() error { return e.err }

func NewSystemBrowser() *SystemBrowser {
	return &SystemBrowser{
		LookupEnv: os.LookupEnv,
		Command: func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		},
		Warning: os.Stderr,
	}
}

func (b *SystemBrowser) Open(ctx context.Context, rawURL string) error {
	lookup := os.LookupEnv
	command := func(ctx context.Context, name string, args ...string) error {
		return exec.CommandContext(ctx, name, args...).Run()
	}
	warning := io.Discard
	if b != nil {
		if b.LookupEnv != nil {
			lookup = b.LookupEnv
		}
		if b.Command != nil {
			command = b.Command
		}
		if b.Warning != nil {
			warning = b.Warning
		}
	}
	display, _ := lookup("DISPLAY")
	waylandDisplay, _ := lookup("WAYLAND_DISPLAY")
	if display == "" && waylandDisplay == "" {
		return nil
	}
	if err := command(ctx, "xdg-open", rawURL); err != nil {
		_, _ = fmt.Fprintln(warning, "browser launch failed; open the authorization URL manually")
	}
	return nil
}

func NewBrowserAuthorizer(client *Client, browser Browser) *BrowserAuthorizer {
	if browser == nil {
		browser = NewSystemBrowser()
	}
	return &BrowserAuthorizer{
		OAuth: client, Browser: browser, Random: rand.Reader, CallbackTimeout: defaultCallbackTimeout,
	}
}

func (a *BrowserAuthorizer) Authorize(ctx context.Context, existing *Registration, callbackInput io.Reader, notice io.Writer) (Registration, TokenSet, error) {
	if a == nil || a.OAuth == nil {
		return Registration{}, TokenSet{}, authError("authorize", "client is missing", errors.New("OAuth client is missing"))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := a.CallbackTimeout
	if timeout <= 0 {
		timeout = defaultCallbackTimeout
	}
	authorizeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if notice == nil {
		notice = io.Discard
	}

	manualReader := newCallbackLineReader(callbackInput)
	reg, callback, err := a.prepareCallback(authorizeCtx, existing)
	if err != nil {
		return Registration{}, TokenSet{}, err
	}
	tokens, attemptErr := a.attempt(authorizeCtx, reg, callback, manualReader, notice, true)
	closeErr := callback.Close()
	if attemptErr == nil {
		if closeErr != nil {
			return reg, TokenSet{}, authError("authorize", "close callback", closeErr)
		}
		return reg, tokens, nil
	}
	if !errors.Is(attemptErr, errPKCEUnsupported) {
		return reg, TokenSet{}, attemptErr
	}

	reg, callback, err = a.prepareCallback(authorizeCtx, &reg)
	if err != nil {
		return reg, TokenSet{}, err
	}
	tokens, attemptErr = a.attempt(authorizeCtx, reg, callback, manualReader, notice, false)
	closeErr = callback.Close()
	if attemptErr != nil {
		return reg, TokenSet{}, attemptErr
	}
	if closeErr != nil {
		return reg, TokenSet{}, authError("authorize", "close callback", closeErr)
	}
	return reg, tokens, nil
}

func (a *BrowserAuthorizer) prepareCallback(ctx context.Context, existing *Registration) (Registration, *Callback, error) {
	if existing != nil && existing.RedirectURI != "" {
		if callback, err := ListenCallback(existing.RedirectURI); err == nil {
			return *existing, callback, nil
		}
	}
	callback, err := ListenCallback("")
	if err != nil {
		return Registration{}, nil, err
	}
	reg, err := a.OAuth.Register(ctx, callback.RedirectURI())
	if err != nil {
		_ = callback.Close()
		return Registration{}, nil, err
	}
	return reg, callback, nil
}

func (a *BrowserAuthorizer) attempt(ctx context.Context, reg Registration, callback *Callback, manualReader *callbackLineReader, notice io.Writer, withPKCE bool) (TokenSet, error) {
	state, err := a.randomValue()
	if err != nil {
		return TokenSet{}, authError("authorize", "generate state", &hiddenCause{message: "random source failed", err: err})
	}
	verifier := ""
	challenge := ""
	if withPKCE {
		verifier, err = a.randomValue()
		if err != nil {
			return TokenSet{}, authError("authorize", "generate PKCE verifier", &hiddenCause{message: "random source failed", err: err})
		}
		digest := sha256.Sum256([]byte(verifier))
		challenge = base64.RawURLEncoding.EncodeToString(digest[:])
	}
	rawURL := a.OAuth.AuthorizationURL(reg, state, challenge)
	if manualReader != nil {
		if err := callback.BeginManual(state); err != nil {
			return TokenSet{}, err
		}
	}
	if _, err := fmt.Fprintln(notice, rawURL); err != nil {
		return TokenSet{}, authError("authorize", "print authorization URL", &hiddenCause{message: "write failed", err: err})
	}
	browser := a.Browser
	if browser == nil {
		browser = NewSystemBrowser()
	}
	if err := browser.Open(ctx, rawURL); err != nil {
		return TokenSet{}, authError("authorize", "open browser", &hiddenCause{message: "browser launch failed", err: err})
	}
	var code string
	if manualReader == nil {
		code, err = callback.Wait(ctx, state)
	} else {
		code, err = waitForPastedCallback(ctx, manualReader, callback.RedirectURI(), state, notice)
	}
	if err != nil {
		return TokenSet{}, err
	}
	return a.OAuth.Exchange(ctx, reg, code, verifier)
}

func (a *BrowserAuthorizer) randomValue() (string, error) {
	reader := a.Random
	if reader == nil {
		reader = rand.Reader
	}
	random := make([]byte, 32)
	if _, err := io.ReadFull(reader, random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}
