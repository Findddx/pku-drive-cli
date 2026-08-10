// Package live contains destructive-operation guards used only by the
// build-tagged PKU acceptance tests.
package live

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/remote"
)

var lowercaseNonce = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Manifest binds live cleanup to the unique directory created by one
// acceptance run.
type Manifest struct {
	Nonce       string `json:"nonce"`
	LibraryPath string `json:"library_path"`
	LibraryID   string `json:"library_id"`
	TestPath    string `json:"test_path"`
	TestID      string `json:"test_id"`
	SmallSize   int64  `json:"small_size"`
	SmallID     string `json:"small_id,omitempty"`
	LargeSize   int64  `json:"large_size"`
	LargeID     string `json:"large_id,omitempty"`
}

// ValidateManifest rejects cleanup manifests that do not identify the exact
// child test directory created beneath one visible library root by the same
// acceptance run.
func ValidateManifest(m Manifest) error {
	if !lowercaseNonce.MatchString(m.Nonce) {
		return errors.New("live manifest nonce must be 32 lowercase hexadecimal characters")
	}
	if !validGNSID(m.TestID) {
		return errors.New("live manifest test ID must be a non-empty gns:// ID")
	}
	if !validGNSID(m.LibraryID) {
		return errors.New("live manifest library ID must be a non-empty gns:// ID")
	}
	if m.TestID == m.LibraryID {
		return errors.New("live manifest test and library IDs must differ")
	}
	if m.SmallSize < 0 {
		return errors.New("live manifest small-file size must be nonnegative")
	}
	if m.LargeSize != 128<<20 {
		return errors.New("live manifest large-file size must be exactly 128 MiB")
	}
	seenIDs := map[string]struct{}{m.LibraryID: {}, m.TestID: {}}
	for _, file := range []struct {
		name string
		id   string
	}{{name: "small", id: m.SmallID}, {name: "large", id: m.LargeID}} {
		if file.id == "" {
			continue
		}
		if !validGNSID(file.id) {
			return fmt.Errorf("live manifest %s-file ID must be a non-empty gns:// ID when present", file.name)
		}
		if _, exists := seenIDs[file.id]; exists {
			return errors.New("live manifest object IDs must be distinct")
		}
		seenIDs[file.id] = struct{}{}
	}

	normalizedLibraryPath, err := remote.Normalize(m.LibraryPath)
	if err != nil || normalizedLibraryPath != m.LibraryPath || normalizedLibraryPath == "/" {
		return errors.New("live manifest library path must be a normalized non-root path")
	}
	if strings.Count(strings.TrimPrefix(m.LibraryPath, "/"), "/") != 0 {
		return errors.New("live manifest library path must contain exactly one visible segment")
	}

	normalizedTestPath, err := remote.Normalize(m.TestPath)
	if err != nil || normalizedTestPath != m.TestPath {
		return errors.New("live manifest test path must be normalized")
	}
	expectedTestPath := m.LibraryPath + "/codex-smoke-" + m.Nonce
	if m.TestPath != expectedTestPath {
		return fmt.Errorf("live manifest test path must equal %q", expectedTestPath)
	}
	return nil
}

// ValidateCleanupEntries permits deletion only when every current child is a
// recorded acceptance file with its exact expected identity, path, type, and
// size. A subset is valid so cleanup remains possible after a partial run.
func ValidateCleanupEntries(m Manifest, entries []anyshare.Item) error {
	if err := ValidateManifest(m); err != nil {
		return err
	}
	type expectedEntry struct {
		id   string
		size int64
	}
	expected := map[string]expectedEntry{
		"小文件.txt":              {id: m.SmallID, size: m.SmallSize},
		"multipart-128MiB.bin": {id: m.LargeID, size: m.LargeSize},
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		want, allowed := expected[entry.Name]
		if !allowed {
			return fmt.Errorf("live cleanup found unexpected entry %q", entry.Name)
		}
		if _, duplicate := seen[entry.Name]; duplicate {
			return fmt.Errorf("live cleanup found duplicate entry %q", entry.Name)
		}
		seen[entry.Name] = struct{}{}
		if want.id == "" || entry.ID != want.id {
			return fmt.Errorf("live cleanup entry %q has an unrecorded ID", entry.Name)
		}
		if entry.Type != "file" {
			return fmt.Errorf("live cleanup entry %q is not a file", entry.Name)
		}
		if entry.Path != m.TestPath+"/"+entry.Name {
			return fmt.Errorf("live cleanup entry %q has an unexpected path", entry.Name)
		}
		if entry.Size != want.size {
			return fmt.Errorf("live cleanup entry %q has an unexpected size", entry.Name)
		}
	}
	return nil
}

// ValidateDeleted confirms that the exact nonce-bound directory no longer
// appears in the already-validated library listing. A same-name replacement
// is deliberately not adopted or deleted.
func ValidateDeleted(m Manifest, entries []anyshare.Item) error {
	if err := ValidateManifest(m); err != nil {
		return err
	}
	wantName := "codex-smoke-" + m.Nonce
	for _, entry := range entries {
		if entry.ID == m.TestID || entry.Name == wantName || entry.Path == m.TestPath {
			return errors.New("live cleanup directory is still present or has been replaced")
		}
	}
	return nil
}

func validGNSID(id string) bool {
	return strings.HasPrefix(id, "gns://") && len(id) > len("gns://")
}
