// Package remote resolves visible AnyShare paths without retaining remote IDs.
package remote

import (
	"errors"
	"strings"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

// Normalize validates an absolute slash-separated remote path and removes
// redundant separators and current-directory components.
func Normalize(path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		return "", invalidPath("remote path must be absolute")
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", invalidPath("remote path contains NUL")
	}

	parts := make([]string, 0)
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", invalidPath("remote path must not contain parent components")
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "/", nil
	}
	return "/" + strings.Join(parts, "/"), nil
}

func invalidPath(message string) error {
	return apperr.Wrap(apperr.Usage, "remote path", message, errors.New("invalid path"))
}
