// Package auth coordinates persisted credentials with OAuth authorization.
package auth

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/config"
	"github.com/Findddx/pku-drive-cli/internal/oauth"
)

const refreshWindow = 5 * time.Minute

// TokenSource supplies an access token, refreshing it when necessary.
type TokenSource interface {
	Token(ctx context.Context, forceRefresh bool) (string, error)
}

// OAuthProtocol is the non-interactive OAuth behavior used by Manager.
type OAuthProtocol interface {
	Refresh(ctx context.Context, reg oauth.Registration, refreshToken string) (oauth.TokenSet, error)
	Revoke(ctx context.Context, reg oauth.Registration, token string) error
}

// InteractiveAuthorizer performs browser-mediated authorization.
type InteractiveAuthorizer interface {
	Authorize(ctx context.Context, existing *oauth.Registration, callbackInput io.Reader, notice io.Writer) (oauth.Registration, oauth.TokenSet, error)
}

// Status describes the locally persisted login state.
type Status struct {
	LoggedIn  bool
	Server    string
	ExpiresAt time.Time
}

// Manager coordinates login state, token refresh, and logout.
type Manager struct {
	Store      *config.Store
	OAuth      OAuthProtocol
	Authorizer InteractiveAuthorizer
	Now        func() time.Time
}

// Login reuses a fresh token, refreshes it, or performs interactive
// authorization when necessary.
func (m *Manager) Login(ctx context.Context, callbackInput io.Reader, notice io.Writer) (Status, error) {
	ctx = nonNilContext(ctx)
	if err := contextError(ctx, "login"); err != nil {
		return Status{}, err
	}
	if m == nil || m.Store == nil {
		return Status{}, localError("login", "credential store is missing", errors.New("credential store is missing"))
	}

	var status Status
	err := m.Store.WithCredentialLock(ctx, func() error {
		credentials, err := m.Store.LoadCredentials()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return localError("login", "load credentials", err)
		}
		if err == nil && credentialFresh(credentials, m.now()) {
			status = credentialStatus(credentials)
			return nil
		}

		if err == nil && usableRegistration(credentials) && credentials.RefreshToken != "" {
			tokens, refreshErr := m.refresh(ctx, credentials)
			if refreshErr == nil {
				updated := credentialsWithTokens(credentials, tokens, m.now())
				if err := m.Store.SaveCredentials(updated); err != nil {
					return localError("login", "save refreshed credentials", err)
				}
				status = credentialStatus(updated)
				return nil
			}
			if errors.Is(refreshErr, context.Canceled) || errors.Is(refreshErr, context.DeadlineExceeded) {
				return interruptedError("login", refreshErr)
			}
		}

		if m.Authorizer == nil {
			return authError("login", "interactive authorizer is missing", errors.New("interactive authorizer is missing"))
		}
		var existing *oauth.Registration
		if err == nil && usableRegistration(credentials) {
			registration := registrationFromCredentials(credentials)
			existing = &registration
		}
		registration, tokens, err := m.Authorizer.Authorize(ctx, existing, callbackInput, notice)
		if err != nil {
			return preserveCategory("login", "authorization failed", err)
		}
		if tokens.AccessToken == "" {
			return authError("login", "authorization response has no access token", errors.New("authorization response has no access token"))
		}
		updated := credentialsFromAuthorization(credentials.Server, registration, tokens, m.now())
		if updated.Server == "" {
			configuration, err := m.Store.LoadConfig()
			if err != nil {
				return localError("login", "load server configuration", err)
			}
			updated.Server = configuration.Server
		}
		if err := m.Store.SaveCredentials(updated); err != nil {
			return localError("login", "save authorized credentials", err)
		}
		status = credentialStatus(updated)
		return nil
	})
	if err != nil {
		return Status{}, lockError("login", err)
	}
	return status, nil
}

// Token returns a fresh access token. It never starts interactive
// authorization.
func (m *Manager) Token(ctx context.Context, forceRefresh bool) (string, error) {
	ctx = nonNilContext(ctx)
	if err := contextError(ctx, "token"); err != nil {
		return "", err
	}
	if m == nil || m.Store == nil {
		return "", localError("token", "credential store is missing", errors.New("credential store is missing"))
	}

	observed, observedErr := m.Store.LoadCredentials()
	var accessToken string
	err := m.Store.WithCredentialLock(ctx, func() error {
		credentials, err := m.Store.LoadCredentials()
		if errors.Is(err, os.ErrNotExist) {
			return authError("token", "login required", errors.New("credentials are missing"))
		}
		if err != nil {
			return localError("token", "load credentials", err)
		}
		changedWhileWaiting := observedErr == nil && credentials != observed
		if credentialFresh(credentials, m.now()) && (!forceRefresh || changedWhileWaiting) {
			accessToken = credentials.AccessToken
			return nil
		}
		if !usableRegistration(credentials) || credentials.RefreshToken == "" {
			return authError("token", "login required", errors.New("credentials cannot be refreshed"))
		}

		tokens, err := m.refresh(ctx, credentials)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return interruptedError("token", err)
			}
			return authError("token", "refresh failed; login required", err)
		}
		updated := credentialsWithTokens(credentials, tokens, m.now())
		if err := m.Store.SaveCredentials(updated); err != nil {
			return localError("token", "save refreshed credentials", err)
		}
		accessToken = updated.AccessToken
		return nil
	})
	if err != nil {
		return "", lockError("token", err)
	}
	return accessToken, nil
}

// Status returns the locally persisted login status without network access.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	ctx = nonNilContext(ctx)
	if err := contextError(ctx, "status"); err != nil {
		return Status{}, err
	}
	if m == nil || m.Store == nil {
		return Status{}, localError("status", "credential store is missing", errors.New("credential store is missing"))
	}
	credentials, err := m.Store.LoadCredentials()
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, localError("status", "load credentials", err)
	}
	return credentialStatus(credentials), nil
}

// Logout revokes persisted tokens before deleting credentials. localOnly skips
// all network access and deletes only the local credential file.
func (m *Manager) Logout(ctx context.Context, localOnly bool) error {
	ctx = nonNilContext(ctx)
	if err := contextError(ctx, "logout"); err != nil {
		return err
	}
	if m == nil || m.Store == nil {
		return localError("logout", "credential store is missing", errors.New("credential store is missing"))
	}

	err := m.Store.WithCredentialLock(ctx, func() error {
		if localOnly {
			if err := m.Store.DeleteCredentials(); err != nil {
				return localError("logout", "delete credentials", err)
			}
			return nil
		}

		credentials, err := m.Store.LoadCredentials()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return localError("logout", "load credentials", err)
		}
		if credentials.AccessToken == "" && credentials.RefreshToken == "" {
			return authError("logout", "credentials cannot be revoked", errors.New("no revocable token is stored"))
		}
		if (credentials.AccessToken != "" || credentials.RefreshToken != "") && !usableRegistration(credentials) {
			return authError("logout", "credentials cannot be revoked", errors.New("client registration is missing"))
		}
		if m.OAuth == nil && (credentials.AccessToken != "" || credentials.RefreshToken != "") {
			return authError("logout", "OAuth client is missing", errors.New("OAuth client is missing"))
		}

		registration := registrationFromCredentials(credentials)
		var accessErr, refreshErr error
		if credentials.AccessToken != "" {
			accessErr = m.OAuth.Revoke(ctx, registration, credentials.AccessToken)
		}
		if credentials.RefreshToken != "" {
			refreshErr = m.OAuth.Revoke(ctx, registration, credentials.RefreshToken)
		}

		authoritativeErr := accessErr
		if credentials.RefreshToken != "" {
			authoritativeErr = refreshErr
		}
		if authoritativeErr != nil {
			return preserveCategory("logout", "token revocation failed", authoritativeErr)
		}
		if err := m.Store.DeleteCredentials(); err != nil {
			return localError("logout", "delete credentials", err)
		}
		return nil
	})
	return lockError("logout", err)
}

func (m *Manager) refresh(ctx context.Context, credentials config.Credentials) (oauth.TokenSet, error) {
	if m.OAuth == nil {
		return oauth.TokenSet{}, errors.New("OAuth client is missing")
	}
	tokens, err := m.OAuth.Refresh(ctx, registrationFromCredentials(credentials), credentials.RefreshToken)
	if err != nil {
		return oauth.TokenSet{}, err
	}
	if tokens.AccessToken == "" {
		return oauth.TokenSet{}, errors.New("refresh response has no access token")
	}
	return tokens, nil
}

func (m *Manager) now() time.Time {
	if m != nil && m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func credentialFresh(credentials config.Credentials, now time.Time) bool {
	return credentials.AccessToken != "" && credentials.ExpiresAt.After(now.Add(refreshWindow))
}

func usableRegistration(credentials config.Credentials) bool {
	return credentials.ClientID != "" && credentials.ClientSecret != "" && credentials.RedirectURI != ""
}

func registrationFromCredentials(credentials config.Credentials) oauth.Registration {
	return oauth.Registration{
		ClientID: credentials.ClientID, ClientSecret: credentials.ClientSecret, RedirectURI: credentials.RedirectURI,
	}
}

func credentialsWithTokens(credentials config.Credentials, tokens oauth.TokenSet, now time.Time) config.Credentials {
	credentials.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		credentials.RefreshToken = tokens.RefreshToken
	}
	credentials.IDToken = tokens.IDToken
	credentials.TokenType = tokens.TokenType
	credentials.ExpiresAt = now.Add(tokens.ExpiresIn)
	return credentials
}

func credentialsFromAuthorization(server string, registration oauth.Registration, tokens oauth.TokenSet, now time.Time) config.Credentials {
	return config.Credentials{
		Server: server, ClientID: registration.ClientID, ClientSecret: registration.ClientSecret,
		RedirectURI: registration.RedirectURI, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		IDToken: tokens.IDToken, TokenType: tokens.TokenType, ExpiresAt: now.Add(tokens.ExpiresIn),
	}
}

func credentialStatus(credentials config.Credentials) Status {
	return Status{LoggedIn: true, Server: credentials.Server, ExpiresAt: credentials.ExpiresAt}
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func contextError(ctx context.Context, op string) error {
	if err := ctx.Err(); err != nil {
		return interruptedError(op, err)
	}
	return nil
}

func interruptedError(op string, err error) error {
	return apperr.Wrap(apperr.Interrupted, op, "interrupted", err)
}

func authError(op, message string, err error) error {
	return apperr.Wrap(apperr.Auth, op, message, err)
}

func localError(op, message string, err error) error {
	return apperr.Wrap(apperr.Local, op, message, err)
}

func preserveCategory(op, message string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return interruptedError(op, err)
	}
	if containsCategory(err, apperr.Network) {
		return apperr.Wrap(apperr.Network, op, message, err)
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return err
	}
	return authError(op, message, err)
}

func lockError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return interruptedError(op, err)
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return err
	}
	return localError(op, "credential lock failed", err)
}

func containsCategory(err error, category apperr.Category) bool {
	if err == nil {
		return false
	}
	if appErr, ok := err.(*apperr.Error); ok && appErr.Category == category {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, nested := range joined.Unwrap() {
			if containsCategory(nested, category) {
				return true
			}
		}
		return false
	}
	return containsCategory(errors.Unwrap(err), category)
}
