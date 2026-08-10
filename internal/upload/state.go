package upload

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/securefs"
)

// CompletedPart records the server result for one uploaded part.
type CompletedPart struct {
	ETag string `json:"etag"`
	Size int64  `json:"size"`
}

// OperationPhase is the durable, secret-free lifecycle marker for an upload.
type OperationPhase string

const (
	PhaseUploading               OperationPhase = "uploading"
	PhaseDirectAttempted         OperationPhase = "direct-attempted"
	PhaseBeginSingleAttempted    OperationPhase = "begin-single-attempted"
	PhaseBeginMultipartAttempted OperationPhase = "begin-multipart-attempted"
	PhaseFinishAttempted         OperationPhase = "finish-attempted"
)

// State is the durable information needed to resume one upload.
type State struct {
	Server      string                `json:"server"`
	RemotePath  string                `json:"remote_path"`
	LocalPath   string                `json:"local_path"`
	DocID       string                `json:"doc_id"`
	Rev         string                `json:"revision"`
	EditedRev   string                `json:"edited_rev,omitempty"`
	Phase       OperationPhase        `json:"phase,omitempty"`
	UploadID    string                `json:"upload_id"`
	Fingerprint Fingerprint           `json:"fingerprint"`
	PartSize    int64                 `json:"part_size"`
	Completed   map[int]CompletedPart `json:"completed"`
	CreatedAt   time.Time             `json:"created_at"`
	UpdatedAt   time.Time             `json:"updated_at"`
}

// Matches reports whether the complete local-file fingerprint is identical.
func (s State) Matches(fingerprint Fingerprint) bool {
	return s.Fingerprint == fingerprint
}

// StateKey maps the caller-validated upload tuple to a path-safe opaque key.
// Lengths are byte lengths encoded as unsigned 64-bit big-endian values.
func StateKey(server, remotePath, absoluteLocalPath string) string {
	hash := sha256.New()
	var length [8]byte
	for _, value := range []string{server, remotePath, absoluteLocalPath} {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// StateStore persists upload state below one absolute private directory.
type StateStore struct {
	dir string
}

// NewStateStore constructs a resume-state store. The directory is validated
// and created by each operation, keeping construction side-effect free.
func NewStateStore(dir string) *StateStore {
	return &StateStore{dir: dir}
}

// Load reads a selected resume-state record. Corrupt JSON is retained for
// diagnosis and returned as an integrity error.
func (s *StateStore) Load(key string) (State, error) {
	path, err := s.entryPath(key, ".json")
	if err != nil {
		return State{}, err
	}
	var state State
	if err := securefs.ReadJSON0600(path, &state); err != nil {
		category := apperr.Local
		if strings.HasPrefix(err.Error(), "decode JSON:") {
			category = apperr.Integrity
		}
		return State{}, apperr.Wrap(category, "load upload state", stateReadMessage(category), err)
	}
	return state, nil
}

// Save atomically writes a selected resume-state record as a regular 0600
// file. No path stored inside state participates in selecting the file.
func (s *StateStore) Save(key string, state State) error {
	path, err := s.entryPath(key, ".json")
	if err != nil {
		return err
	}
	if err := securefs.WriteJSONAtomic(path, state); err != nil {
		return apperr.Wrap(apperr.Local, "save upload state", "local state operation failed", err)
	}
	return nil
}

// Remove idempotently removes exactly one selected resume-state record.
func (s *StateStore) Remove(key string) error {
	path, err := s.entryPath(key, ".json")
	if err != nil {
		return err
	}
	if err := securefs.Remove0600(path); err != nil {
		return apperr.Wrap(apperr.Local, "remove upload state", "local state operation failed", err)
	}
	return nil
}

// WithUploadLock holds the selected upload's separate lock file throughout fn.
// Save can therefore run inside fn without trying to reacquire this lock.
func (s *StateStore) WithUploadLock(ctx context.Context, key string, fn func() error) error {
	path, err := s.entryPath(key, ".lock")
	if err != nil {
		return err
	}
	if fn == nil {
		return apperr.Wrap(apperr.Usage, "lock upload state", "missing lock callback", errors.New("nil callback"))
	}
	var callbackErr error
	err = securefs.WithLock(ctx, path, func() error {
		callbackErr = fn()
		return callbackErr
	})
	if callbackErr != nil {
		return err
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return apperr.Wrap(apperr.Interrupted, "lock upload state", "interrupted", err)
	}
	return apperr.Wrap(apperr.Local, "lock upload state", "local state operation failed", err)
}

func (s *StateStore) entryPath(key, suffix string) (string, error) {
	if !validStateKey(key) {
		return "", apperr.Wrap(apperr.Usage, "upload state key", "invalid key", errors.New("expected lowercase SHA-256 hexadecimal"))
	}
	if s == nil {
		return "", apperr.Wrap(apperr.Local, "open upload state directory", "local state operation failed", errors.New("nil state store"))
	}
	dir, err := securefs.EnsurePrivateDir(s.dir)
	if err != nil {
		return "", apperr.Wrap(apperr.Local, "open upload state directory", "local state operation failed", err)
	}
	if err := dir.Close(); err != nil {
		return "", apperr.Wrap(apperr.Local, "close upload state directory", "local state operation failed", err)
	}
	return filepath.Join(s.dir, key+suffix), nil
}

func validStateKey(key string) bool {
	if len(key) != sha256.Size*2 {
		return false
	}
	for _, character := range []byte(key) {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func stateReadMessage(category apperr.Category) string {
	if category == apperr.Integrity {
		return "resume state is corrupt"
	}
	return "local state operation failed"
}
