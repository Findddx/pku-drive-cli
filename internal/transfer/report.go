// Package transfer coordinates directory-level recursive downloads and
// uploads on top of the single-file transfer kernels.
package transfer

import (
	"github.com/Findddx/pku-drive-cli/internal/download"
	"github.com/Findddx/pku-drive-cli/internal/upload"
)

// Options controls one tree transfer.
type Options struct {
	Overwrite bool
	Jobs      int // 1..64, validated by the CLI before being passed in
}

// SkipReason is why one tree entry was not transferred.
type SkipReason string

const (
	SkipExists  SkipReason = "exists"  // both directions: same-named destination exists and --overwrite is unset
	SkipSymlink SkipReason = "symlink" // upload: local symbolic link
)

// Skipped records one tree entry that was not transferred.
type Skipped struct {
	Path   string // display path relative to the root
	Reason SkipReason
}

// CompletedDownload pairs one verified download with its relative path.
type CompletedDownload struct {
	Relative   string // path relative to the root; sort key, never rendered
	RemotePath string // full remote path (WalkFiles Item.Path)
	Result     download.Result
}

// CompletedUpload pairs one verified upload with its relative path.
type CompletedUpload struct {
	Relative string
	Result   upload.Result // RemotePath is carried by the uploader result
}

// DownloadReport is the aggregate recursive-download outcome.
type DownloadReport struct {
	Completed []CompletedDownload
	Skipped   []Skipped
}

// UploadReport is the aggregate recursive-upload outcome.
type UploadReport struct {
	Completed []CompletedUpload
	Skipped   []Skipped
}
