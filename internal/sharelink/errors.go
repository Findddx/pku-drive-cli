// Package sharelink opens PKU AnyShare links as isolated, navigable sessions.
package sharelink

import (
	"context"
	"errors"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

var (
	// ErrInvalidLink marks a URL that is not an exact same-origin share link.
	ErrInvalidLink = errors.New("invalid share link")
	// ErrAuthenticationRequired marks a real-name link that needs a saved login.
	ErrAuthenticationRequired = errors.New("login required for shared link")
	// ErrExpired marks a link whose configured expiry is in the past.
	ErrExpired = errors.New("shared link expired")
	// ErrPasswordRequired marks an anonymous link that requires a password.
	ErrPasswordRequired = errors.New("shared link password required")
	// ErrMobileVerificationRequired marks a link that requires mobile verification.
	ErrMobileVerificationRequired = errors.New("shared link mobile verification required")
	// ErrNotFound marks a missing shared link or relative entry.
	ErrNotFound = errors.New("shared entry not found")
	// ErrNotFile marks an entry that cannot be downloaded as a file.
	ErrNotFile = errors.New("shared entry is not a file")
	// ErrNotDirectory marks an entry that cannot be listed as a directory.
	ErrNotDirectory = errors.New("shared entry is not a directory")
)

func invalidLinkError() error {
	return apperr.Wrap(apperr.Usage, "share link", "invalid URL", ErrInvalidLink)
}

func invalidPathError() error {
	return apperr.Wrap(apperr.Usage, "share path", "invalid relative path", errors.New("invalid path"))
}

func authRequiredError() error {
	return apperr.Wrap(apperr.Auth, "share link", "login required", ErrAuthenticationRequired)
}

func passwordRequiredError() error {
	return apperr.Wrap(apperr.Auth, "share link", "password-protected link", ErrPasswordRequired)
}

func mobileVerificationRequiredError() error {
	return apperr.Wrap(apperr.Auth, "share link", "mobile verification required", ErrMobileVerificationRequired)
}

func expiredError() error {
	return apperr.Wrap(apperr.Remote, "share link", "link expired", ErrExpired)
}

func notFoundError() error {
	return apperr.Wrap(apperr.Remote, "share path", "entry not found", ErrNotFound)
}

func notFileError() error {
	return apperr.Wrap(apperr.Usage, "share path", "entry is not a file", ErrNotFile)
}

func notDirectoryError() error {
	return apperr.Wrap(apperr.Usage, "share path", "entry is not a directory", ErrNotDirectory)
}

func invalidRemoteError(message string) error {
	return apperr.Wrap(apperr.Remote, "share link", message, errors.New("invalid server response"))
}

func requestError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return apperr.Wrap(apperr.Interrupted, "share link", "request interrupted", err)
		}
	}
	return apperr.Wrap(apperr.Network, "share link", "request failed", errors.New("transport error"))
}

func statusError(status int) error {
	switch status {
	case 401, 403:
		return authRequiredError()
	case 404:
		return apperr.Wrap(apperr.Remote, "share link", "link not found", ErrNotFound)
	case 410:
		return expiredError()
	default:
		return apperr.Wrap(apperr.Remote, "share link", "server rejected request", errors.New("remote request failed"))
	}
}

func classifyAPIError(err error) error {
	if err == nil {
		return nil
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) && appErr != nil && appErr.Category == apperr.Auth {
		return authRequiredError()
	}
	return err
}
