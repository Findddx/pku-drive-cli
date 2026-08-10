package auth_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/auth"
	"github.com/Findddx/pku-drive-cli/internal/config"
	"github.com/Findddx/pku-drive-cli/internal/oauth"
)

var fixedNow = time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)

type managerHarness struct {
	manager    *auth.Manager
	store      *config.Store
	paths      config.Paths
	oauth      *fakeOAuth
	authorizer *fakeAuthorizer
}

type fakeOAuth struct {
	mu             sync.Mutex
	refreshCalls   int
	refreshRegs    []oauth.Registration
	refreshTokens  []string
	refreshResult  oauth.TokenSet
	refreshErr     error
	refreshStarted chan struct{}
	refreshRelease chan struct{}
	startOnce      sync.Once
	revokeOrder    []string
	revokeErrors   map[string]error
}

func (f *fakeOAuth) Refresh(ctx context.Context, reg oauth.Registration, refreshToken string) (oauth.TokenSet, error) {
	f.mu.Lock()
	f.refreshCalls++
	f.refreshRegs = append(f.refreshRegs, reg)
	f.refreshTokens = append(f.refreshTokens, refreshToken)
	result, err := f.refreshResult, f.refreshErr
	started, release := f.refreshStarted, f.refreshRelease
	f.mu.Unlock()
	if started != nil {
		f.startOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return oauth.TokenSet{}, ctx.Err()
		}
	}
	if err != nil {
		return oauth.TokenSet{}, err
	}
	return result, nil
}

func (f *fakeOAuth) Revoke(_ context.Context, _ oauth.Registration, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeOrder = append(f.revokeOrder, token)
	return f.revokeErrors[token]
}

func (f *fakeOAuth) RefreshCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls
}

func (f *fakeOAuth) RevokeOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revokeOrder...)
}

type fakeAuthorizer struct {
	mu           sync.Mutex
	calls        int
	existing     []*oauth.Registration
	inputs       []io.Reader
	registration oauth.Registration
	tokens       oauth.TokenSet
	err          error
}

func (f *fakeAuthorizer) Authorize(_ context.Context, existing *oauth.Registration, input io.Reader, _ io.Writer) (oauth.Registration, oauth.TokenSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.inputs = append(f.inputs, input)
	if existing == nil {
		f.existing = append(f.existing, nil)
	} else {
		copyOfExisting := *existing
		f.existing = append(f.existing, &copyOfExisting)
	}
	return f.registration, f.tokens, f.err
}

func (f *fakeAuthorizer) Input(index int) io.Reader {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inputs[index]
}

func (f *fakeAuthorizer) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeAuthorizer) Existing(index int) *oauth.Registration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.existing[index] == nil {
		return nil
	}
	copyOfExisting := *f.existing[index]
	return &copyOfExisting
}

func newManagerHarness(t *testing.T, credentials *config.Credentials) *managerHarness {
	t.Helper()
	root := t.TempDir()
	paths := config.Paths{
		ConfigDir:       filepath.Join(root, "config", "pku-drive-cli"),
		ConfigFile:      filepath.Join(root, "config", "pku-drive-cli", "config.json"),
		CredentialsFile: filepath.Join(root, "config", "pku-drive-cli", "credentials.json"),
		CredentialLock:  filepath.Join(root, "config", "pku-drive-cli", "credentials.lock"),
		UploadStateDir:  filepath.Join(root, "state", "pku-drive-cli", "uploads"),
	}
	store := config.NewStore(paths)
	if credentials != nil {
		if err := store.SaveCredentials(*credentials); err != nil {
			t.Fatal(err)
		}
	}
	protocol := &fakeOAuth{
		refreshResult: oauth.TokenSet{
			AccessToken: "new-access", RefreshToken: "new-refresh", IDToken: "new-id",
			TokenType: "Bearer", ExpiresIn: time.Hour,
		},
		revokeErrors: make(map[string]error),
	}
	authorizer := &fakeAuthorizer{
		registration: oauth.Registration{ClientID: "registered-client", ClientSecret: "registered-secret", RedirectURI: "http://127.0.0.1:43123/callback"},
		tokens: oauth.TokenSet{
			AccessToken: "authorized-access", RefreshToken: "authorized-refresh", IDToken: "authorized-id",
			TokenType: "Bearer", ExpiresIn: 2 * time.Hour,
		},
	}
	return &managerHarness{
		manager: &auth.Manager{Store: store, OAuth: protocol, Authorizer: authorizer, Now: func() time.Time { return fixedNow }},
		store:   store, paths: paths, oauth: protocol, authorizer: authorizer,
	}
}

func validCredentials() *config.Credentials {
	return &config.Credentials{
		Server: config.DefaultServer, ClientID: "saved-client", ClientSecret: "saved-secret",
		RedirectURI: "http://127.0.0.1:41234/callback", AccessToken: "old-access",
		RefreshToken: "old-refresh", IDToken: "old-id", TokenType: "Bearer", ExpiresAt: fixedNow.Add(time.Hour),
	}
}

func expiredCredentials() *config.Credentials {
	credentials := validCredentials()
	credentials.ExpiresAt = fixedNow.Add(-time.Minute)
	return credentials
}

func requireCategory(t *testing.T, err error, want apperr.Category, wantExit int) {
	t.Helper()
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != want {
		t.Fatalf("error = %#v, want category %q", err, want)
	}
	if got := apperr.ExitCode(err); got != wantExit {
		t.Fatalf("exit code = %d, want %d", got, wantExit)
	}
}

func TestTokenRefreshesOnceUnderLock(t *testing.T) {
	h := newManagerHarness(t, expiredCredentials())
	h.oauth.refreshStarted = make(chan struct{})
	h.oauth.refreshRelease = make(chan struct{})
	results := make(chan string, 2)
	errs := make(chan error, 2)
	go func() {
		token, err := h.manager.Token(context.Background(), false)
		results <- token
		errs <- err
	}()
	<-h.oauth.refreshStarted
	go func() {
		token, err := h.manager.Token(context.Background(), false)
		results <- token
		errs <- err
	}()
	close(h.oauth.refreshRelease)
	if first, second := <-results, <-results; first != "new-access" || second != "new-access" {
		t.Fatalf("tokens=%q,%q", first, second)
	}
	if first, second := <-errs, <-errs; first != nil || second != nil {
		t.Fatalf("errors=%v,%v", first, second)
	}
	if got := h.oauth.RefreshCalls(); got != 1 {
		t.Fatalf("refresh calls=%d", got)
	}
	stored, err := h.store.LoadCredentials()
	if err != nil || stored.AccessToken != "new-access" {
		t.Fatalf("stored access token not refreshed: err=%v", err)
	}
}

func TestConcurrentForcedTokensReuseRefreshCompletedWhileWaiting(t *testing.T) {
	h := newManagerHarness(t, expiredCredentials())
	h.oauth.refreshStarted = make(chan struct{})
	h.oauth.refreshRelease = make(chan struct{})
	results := make(chan string, 2)
	errs := make(chan error, 2)

	go func() {
		token, err := h.manager.Token(context.Background(), true)
		results <- token
		errs <- err
	}()
	<-h.oauth.refreshStarted

	watchFD, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(watchFD) })
	if _, err := syscall.InotifyAddWatch(watchFD, h.paths.CredentialsFile, syscall.IN_OPEN); err != nil {
		t.Fatal(err)
	}

	go func() {
		token, err := h.manager.Token(context.Background(), true)
		results <- token
		errs <- err
	}()
	observed, err := waitForInotifyEvent(watchFD, time.Second)
	if err != nil {
		close(h.oauth.refreshRelease)
		t.Fatal(err)
	}
	if !observed {
		close(h.oauth.refreshRelease)
		<-results
		<-results
		<-errs
		<-errs
		t.Fatal("second forced caller did not observe credentials before waiting for the lock")
	}
	close(h.oauth.refreshRelease)

	if first, second := <-results, <-results; first != "new-access" || second != "new-access" {
		t.Fatalf("tokens=%q,%q", first, second)
	}
	if first, second := <-errs, <-errs; first != nil || second != nil {
		t.Fatalf("errors=%v,%v", first, second)
	}
	if got := h.oauth.RefreshCalls(); got != 1 {
		t.Fatalf("refresh calls=%d, want 1", got)
	}
}

func waitForInotifyEvent(fd int, timeout time.Duration) (bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	buffer := make([]byte, syscall.SizeofInotifyEvent*4)
	for {
		if count, err := syscall.Read(fd, buffer); count >= syscall.SizeofInotifyEvent {
			return true, nil
		} else if err != nil && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return false, err
		}
		select {
		case <-deadline.C:
			return false, nil
		case <-poll.C:
		}
	}
}

func TestLoginReusesValidCredential(t *testing.T) {
	h := newManagerHarness(t, validCredentials())
	status, err := h.manager.Login(context.Background(), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if status != (auth.Status{LoggedIn: true, Server: config.DefaultServer, ExpiresAt: fixedNow.Add(time.Hour)}) {
		t.Fatalf("status = %+v", status)
	}
	if h.oauth.RefreshCalls() != 0 || h.authorizer.Calls() != 0 {
		t.Fatal("valid login performed OAuth work")
	}
}

func TestTokenRefreshBoundaryAndForceRefresh(t *testing.T) {
	tests := []struct {
		name         string
		expiresAt    time.Time
		forceRefresh bool
		wantToken    string
		wantCalls    int
	}{
		{name: "more than five minutes remains", expiresAt: fixedNow.Add(5*time.Minute + time.Nanosecond), wantToken: "old-access"},
		{name: "exactly five minutes remains", expiresAt: fixedNow.Add(5 * time.Minute), wantToken: "new-access", wantCalls: 1},
		{name: "forced valid token", expiresAt: fixedNow.Add(time.Hour), forceRefresh: true, wantToken: "new-access", wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credentials := validCredentials()
			credentials.ExpiresAt = test.expiresAt
			h := newManagerHarness(t, credentials)
			got, err := h.manager.Token(context.Background(), test.forceRefresh)
			if err != nil || got != test.wantToken {
				t.Fatalf("Token() = %q, %v; want %q, nil", got, err, test.wantToken)
			}
			if got := h.oauth.RefreshCalls(); got != test.wantCalls {
				t.Fatalf("refresh calls = %d, want %d", got, test.wantCalls)
			}
		})
	}
}

func TestRefreshPersistsTokensAndPreservesFileMode(t *testing.T) {
	h := newManagerHarness(t, expiredCredentials())
	h.oauth.refreshResult.RefreshToken = ""
	if token, err := h.manager.Token(context.Background(), false); err != nil || token != "new-access" {
		t.Fatalf("Token() = %q, %v", token, err)
	}
	stored, err := h.store.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "old-refresh" || stored.AccessToken != "new-access" || stored.IDToken != "new-id" || stored.TokenType != "Bearer" || !stored.ExpiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("stored credential fields were not updated while preserving refresh token: %+v", stored)
	}
	info, err := os.Stat(h.paths.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("credentials mode = %o, want 600", got)
	}
}

func TestLoginRefreshesExpiredCredential(t *testing.T) {
	h := newManagerHarness(t, expiredCredentials())
	status, err := h.manager.Login(context.Background(), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !status.LoggedIn || !status.ExpiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("status = %+v", status)
	}
	if h.oauth.RefreshCalls() != 1 || h.authorizer.Calls() != 0 {
		t.Fatalf("refresh calls=%d authorizer calls=%d", h.oauth.RefreshCalls(), h.authorizer.Calls())
	}
}

func TestLoginReauthorizesWithSavedRegistrationAfterRefreshFailure(t *testing.T) {
	h := newManagerHarness(t, expiredCredentials())
	h.oauth.refreshErr = errors.New("fixture invalid grant")
	callbackInput := strings.NewReader("fake callback input")
	if _, err := h.manager.Login(context.Background(), callbackInput, io.Discard); err != nil {
		t.Fatal(err)
	}
	if h.authorizer.Calls() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.authorizer.Calls())
	}
	want := oauth.Registration{ClientID: "saved-client", ClientSecret: "saved-secret", RedirectURI: "http://127.0.0.1:41234/callback"}
	if got := h.authorizer.Existing(0); got == nil || *got != want {
		t.Fatalf("existing registration = %+v, want saved registration", got)
	}
	if got := h.authorizer.Input(0); got != callbackInput {
		t.Fatalf("authorizer callback input = %T, want exact reader", got)
	}
	stored, err := h.store.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.ClientID != "registered-client" || stored.AccessToken != "authorized-access" || stored.RefreshToken != "authorized-refresh" || !stored.ExpiresAt.Equal(fixedNow.Add(2*time.Hour)) {
		t.Fatalf("reauthorized credentials not persisted: %+v", stored)
	}
}

func TestLoginDynamicallyRegistersWithoutUsableClient(t *testing.T) {
	credentials := expiredCredentials()
	credentials.ClientSecret = ""
	h := newManagerHarness(t, credentials)
	if _, err := h.manager.Login(context.Background(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if h.oauth.RefreshCalls() != 0 {
		t.Fatalf("refresh calls = %d, want 0", h.oauth.RefreshCalls())
	}
	if h.authorizer.Calls() != 1 || h.authorizer.Existing(0) != nil {
		t.Fatal("login did not request dynamic client registration")
	}
}

func TestMissingCredentialsBehaviors(t *testing.T) {
	h := newManagerHarness(t, nil)
	status, err := h.manager.Status(context.Background())
	if err != nil || status.LoggedIn {
		t.Fatalf("Status() = %+v, %v; want logged out", status, err)
	}
	if _, err := h.manager.Token(context.Background(), false); err == nil {
		t.Fatal("Token() error = nil, want login required")
	} else {
		requireCategory(t, err, apperr.Auth, 3)
	}
	if err := h.manager.Logout(context.Background(), false); err != nil {
		t.Fatalf("Logout() missing credentials = %v", err)
	}
	if _, err := h.manager.Login(context.Background(), nil, io.Discard); err != nil {
		t.Fatalf("Login() missing credentials = %v", err)
	}
	if h.authorizer.Calls() != 1 || h.authorizer.Existing(0) != nil {
		t.Fatal("missing-credential login did not dynamically register")
	}
}

func TestTokenTerminalRefreshErrorsAreStable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want apperr.Category
		exit int
	}{
		{name: "invalid grant", err: apperr.Wrap(apperr.Auth, "oauth refresh", "request rejected", errors.New("fixture invalid grant")), want: apperr.Auth, exit: 3},
		{name: "exhausted transport", err: apperr.Wrap(apperr.Network, "http", "request failed", errors.New("fixture transport exhausted")), want: apperr.Auth, exit: 3},
		{name: "server failure", err: apperr.Wrap(apperr.Remote, "oauth refresh", "request rejected", errors.New("fixture server failure")), want: apperr.Auth, exit: 3},
		{name: "cancellation", err: context.Canceled, want: apperr.Interrupted, exit: 130},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newManagerHarness(t, expiredCredentials())
			h.oauth.refreshErr = test.err
			if _, err := h.manager.Token(context.Background(), false); err == nil {
				t.Fatal("Token() error = nil")
			} else {
				requireCategory(t, err, test.want, test.exit)
			}
			if h.authorizer.Calls() != 0 {
				t.Fatal("Token() launched interactive authorization")
			}
		})
	}
}

func TestInvalidRefreshResponseIsNeverPersisted(t *testing.T) {
	t.Run("token returns terminal auth failure", func(t *testing.T) {
		h := newManagerHarness(t, expiredCredentials())
		h.oauth.refreshResult.AccessToken = ""
		if _, err := h.manager.Token(context.Background(), false); err == nil {
			t.Fatal("Token() error = nil, want invalid refresh failure")
		} else {
			requireCategory(t, err, apperr.Auth, 3)
		}
		stored, err := h.store.LoadCredentials()
		if err != nil || stored.AccessToken != "old-access" {
			t.Fatalf("invalid refresh response changed stored credentials: err=%v", err)
		}
	})

	t.Run("login reauthorizes", func(t *testing.T) {
		h := newManagerHarness(t, expiredCredentials())
		h.oauth.refreshResult.AccessToken = ""
		if _, err := h.manager.Login(context.Background(), nil, io.Discard); err != nil {
			t.Fatal(err)
		}
		if h.authorizer.Calls() != 1 || h.authorizer.Existing(0) == nil {
			t.Fatal("invalid refresh response did not reauthorize with saved client")
		}
		stored, err := h.store.LoadCredentials()
		if err != nil || stored.AccessToken != "authorized-access" {
			t.Fatalf("reauthorized credential not persisted: err=%v", err)
		}
	})
}

func TestLoginRejectsAuthorizationWithoutAccessToken(t *testing.T) {
	h := newManagerHarness(t, expiredCredentials())
	h.oauth.refreshErr = errors.New("fixture invalid grant")
	h.authorizer.tokens.AccessToken = ""
	before, err := os.ReadFile(h.paths.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}

	_, err = h.manager.Login(context.Background(), nil, io.Discard)
	requireCategory(t, err, apperr.Auth, 3)
	if h.authorizer.Calls() != 1 || h.authorizer.Existing(0) == nil {
		t.Fatal("login did not reuse saved registration before rejecting invalid authorization")
	}
	after, err := os.ReadFile(h.paths.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("invalid authorization response changed stored credentials")
	}
	stored, err := h.store.LoadCredentials()
	if err != nil || stored != *expiredCredentials() {
		t.Fatalf("prior credential was not preserved: err=%v", err)
	}
}

func TestContextCancellationIsInterruptedAcrossManagerOperations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name string
		run  func(*managerHarness) error
	}{
		{name: "login", run: func(h *managerHarness) error { _, err := h.manager.Login(ctx, nil, io.Discard); return err }},
		{name: "token", run: func(h *managerHarness) error { _, err := h.manager.Token(ctx, false); return err }},
		{name: "status", run: func(h *managerHarness) error { _, err := h.manager.Status(ctx); return err }},
		{name: "logout", run: func(h *managerHarness) error { return h.manager.Logout(ctx, false) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newManagerHarness(t, validCredentials())
			requireCategory(t, test.run(h), apperr.Interrupted, 130)
		})
	}
}

func TestLoginPreservesNonRefreshErrorCategory(t *testing.T) {
	h := newManagerHarness(t, expiredCredentials())
	h.oauth.refreshErr = errors.New("fixture invalid grant")
	h.authorizer.err = apperr.Wrap(apperr.Auth, "oauth authorize", "request failed",
		apperr.Wrap(apperr.Network, "http", "request failed", errors.New("fixture network down")))
	_, err := h.manager.Login(context.Background(), nil, io.Discard)
	requireCategory(t, err, apperr.Network, 5)
}

func TestLogoutRevokesAccessThenRefreshAndDeletes(t *testing.T) {
	h := newManagerHarness(t, validCredentials())
	if err := h.manager.Logout(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := h.oauth.RevokeOrder(); len(got) != 2 || got[0] != "old-access" || got[1] != "old-refresh" {
		t.Fatalf("revoke order = %q, want access then refresh", got)
	}
	if _, err := h.store.LoadCredentials(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials still exist: %v", err)
	}
}

func TestLogoutRefreshRevocationIsAuthoritative(t *testing.T) {
	tests := []struct {
		name          string
		revokeErrors  map[string]error
		wantErr       bool
		wantRemaining bool
	}{
		{name: "access failure then refresh success", revokeErrors: map[string]error{"old-access": errors.New("fixture access revoke failure")}},
		{name: "refresh failure preserves credential", revokeErrors: map[string]error{"old-refresh": errors.New("fixture refresh revoke failure")}, wantErr: true, wantRemaining: true},
		{name: "both failures preserve credential", revokeErrors: map[string]error{"old-access": errors.New("fixture access revoke failure"), "old-refresh": errors.New("fixture refresh revoke failure")}, wantErr: true, wantRemaining: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newManagerHarness(t, validCredentials())
			h.oauth.revokeErrors = test.revokeErrors
			err := h.manager.Logout(context.Background(), false)
			if (err != nil) != test.wantErr {
				t.Fatalf("Logout() error = %v, wantErr %v", err, test.wantErr)
			}
			if got := h.oauth.RevokeOrder(); len(got) != 2 || got[0] != "old-access" || got[1] != "old-refresh" {
				t.Fatalf("revoke order = %q", got)
			}
			_, loadErr := h.store.LoadCredentials()
			if test.wantRemaining && loadErr != nil {
				t.Fatalf("credentials removed after authoritative failure: %v", loadErr)
			}
			if !test.wantRemaining && !errors.Is(loadErr, os.ErrNotExist) {
				t.Fatalf("credentials remain after authoritative success: %v", loadErr)
			}
		})
	}
}

func TestLogoutAccessRevocationIsAuthoritativeWithoutRefreshToken(t *testing.T) {
	credentials := validCredentials()
	credentials.RefreshToken = ""
	h := newManagerHarness(t, credentials)
	h.oauth.revokeErrors["old-access"] = errors.New("fixture access revoke failure")
	if err := h.manager.Logout(context.Background(), false); err == nil {
		t.Fatal("Logout() error = nil, want access revoke failure")
	}
	if got := h.oauth.RevokeOrder(); len(got) != 1 || got[0] != "old-access" {
		t.Fatalf("revoke order = %q", got)
	}
	if _, err := h.store.LoadCredentials(); err != nil {
		t.Fatalf("credentials removed after access revoke failure: %v", err)
	}
}

func TestNormalLogoutPreservesTokenlessCredential(t *testing.T) {
	credentials := validCredentials()
	credentials.AccessToken = ""
	credentials.RefreshToken = ""
	h := newManagerHarness(t, credentials)
	before, err := os.ReadFile(h.paths.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}

	err = h.manager.Logout(context.Background(), false)
	requireCategory(t, err, apperr.Auth, 3)
	if got := h.oauth.RevokeOrder(); len(got) != 0 {
		t.Fatalf("tokenless logout revoke order = %q, want none", got)
	}
	after, err := os.ReadFile(h.paths.CredentialsFile)
	if err != nil {
		t.Fatalf("tokenless credentials were removed: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("tokenless credentials changed after normal logout")
	}
}

func TestLogoutLocalOnlySkipsNetworkAndDeletes(t *testing.T) {
	h := newManagerHarness(t, validCredentials())
	h.oauth.revokeErrors["old-refresh"] = errors.New("fixture should not be observed")
	if err := h.manager.Logout(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := h.oauth.RevokeOrder(); len(got) != 0 {
		t.Fatalf("local-only revoke order = %q, want none", got)
	}
	if _, err := h.store.LoadCredentials(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials still exist: %v", err)
	}
}

func TestLogoutNetworkFailureRemainsNetworkCategory(t *testing.T) {
	h := newManagerHarness(t, validCredentials())
	h.oauth.revokeErrors["old-refresh"] = apperr.Wrap(apperr.Auth, "oauth revoke", "request failed",
		apperr.Wrap(apperr.Network, "http", "request failed", errors.New("fixture transport failure")))
	err := h.manager.Logout(context.Background(), false)
	requireCategory(t, err, apperr.Network, 5)
	if _, loadErr := h.store.LoadCredentials(); loadErr != nil {
		t.Fatalf("credentials removed after network failure: %v", loadErr)
	}
}

func TestStatusReturnsStoredServerAndExpiry(t *testing.T) {
	h := newManagerHarness(t, validCredentials())
	got, err := h.manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := auth.Status{LoggedIn: true, Server: config.DefaultServer, ExpiresAt: fixedNow.Add(time.Hour)}
	if got != want {
		t.Fatalf("Status() = %+v, want %+v", got, want)
	}
}
