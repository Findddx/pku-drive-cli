package cli

import (
	"context"
	"io"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/upload"
)

type StatusResult struct {
	LoggedIn  bool       `json:"logged_in"`
	Server    string     `json:"server"`
	Account   string     `json:"account,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type ItemResult struct {
	Name       string `json:"name"`
	RemotePath string `json:"remote_path"`
	Type       string `json:"type"`
	RemoteID   string `json:"remote_id"`
	Size       int64  `json:"size"`
	Modified   int64  `json:"modified,omitempty"`
}

type PutResult struct {
	RemotePath    string `json:"remote_path"`
	RemoteID      string `json:"remote_id"`
	Revision      string `json:"revision,omitempty"`
	Size          int64  `json:"size"`
	Resumed       bool   `json:"resumed"`
	Instant       bool   `json:"instant"`
	Multipart     bool   `json:"multipart"`
	PartsTotal    int    `json:"parts_total"`
	PartsResumed  int    `json:"parts_resumed"`
	PartsUploaded int    `json:"parts_uploaded"`
}

type GetResult struct {
	RemotePath string `json:"remote_path"`
	RemoteID   string `json:"remote_id"`
	Revision   string `json:"revision,omitempty"`
	LocalPath  string `json:"local_path"`
	Size       int64  `json:"size"`
}

type DeleteResult struct {
	RemotePath    string `json:"remote_path"`
	RemoteID      string `json:"remote_id"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	PendingReview bool   `json:"pending_review"`
}

// Dependencies contains the external command operations.
type Dependencies struct {
	Login  func(context.Context, io.Reader, io.Writer, bool) error
	Status func(context.Context) (StatusResult, error)
	List   func(context.Context, string) ([]ItemResult, error)
	Mkdir  func(context.Context, string, bool) (ItemResult, error)
	Put    func(context.Context, string, string, bool, upload.Progress) (PutResult, error)
	Get    func(context.Context, string, string, bool, upload.Progress) (GetResult, error)
	Delete func(context.Context, string, bool) (DeleteResult, error)
	Logout func(context.Context, bool) error
}
