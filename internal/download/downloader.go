// Package download coordinates verified AnyShare object downloads and atomic
// local installation.
package download

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

const downloadBufferSize = 256 * 1024

// Progress receives aggregate byte progress for one download.
type Progress interface {
	Started(total int64)
	Advanced(delta int64)
	Finished()
}

// API is the exact AnyShare control- and object-plane surface needed by a
// Downloader.
type API interface {
	FileMetadata(context.Context, string, string) (anyshare.Item, error)
	AuthorizeDownload(context.Context, string, string, string) (anyshare.SignedRequest, error)
	GetSigned(context.Context, anyshare.SignedRequest) (anyshare.DownloadStream, error)
}

// Result describes a successfully verified local file.
type Result struct {
	LocalPath string
	RemoteID  string
	Revision  string
	Size      int64
}

// Downloader verifies an exact remote revision before transferring it.
type Downloader struct {
	API API
}

// NewDownloader constructs a download coordinator.
func NewDownloader(api API) *Downloader { return &Downloader{API: api} }

// Get downloads item to the exact localPath. The transfer is written to a
// mode-0600 temporary file in the destination directory and becomes visible
// only after size and optional MD5 verification succeed.
func (d *Downloader) Get(ctx context.Context, item anyshare.Item, localPath string, overwrite bool, progress Progress) (result Result, returnErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if d == nil || d.API == nil {
		return Result{}, downloadError(apperr.Local, "configure download", errors.New("missing download dependency"))
	}
	if err := validateDownloadItem(item); err != nil {
		return Result{}, downloadError(apperr.Remote, "select download", errors.New("remote path is not an exact file revision"))
	}
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return Result{}, downloadError(apperr.Local, "resolve download destination", err)
	}
	target, err := openDownloadTarget(abs, overwrite)
	if err != nil {
		return Result{}, downloadError(apperr.Local, "inspect download destination", err)
	}
	return d.getToTarget(ctx, item, abs, target, progress)
}

// GetAt downloads item below a descriptor-bound destination root. relativePath
// is a normalized relative file path; missing parent directories are created
// mode 0700 without following symbolic links.
func (d *Downloader) GetAt(ctx context.Context, item anyshare.Item, root *DestinationRoot, relativePath string, overwrite bool, progress Progress) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if d == nil || d.API == nil {
		return Result{}, downloadError(apperr.Local, "configure download", errors.New("missing download dependency"))
	}
	if err := validateDownloadItem(item); err != nil {
		return Result{}, downloadError(apperr.Remote, "select download", errors.New("remote path is not an exact file revision"))
	}
	target, displayPath, err := root.openTarget(relativePath, overwrite)
	if err != nil {
		return Result{}, downloadError(apperr.Local, "inspect download destination", err)
	}
	return d.getToTarget(ctx, item, displayPath, target, progress)
}

func (d *Downloader) getToTarget(ctx context.Context, item anyshare.Item, displayPath string, target *downloadTarget, progress Progress) (result Result, returnErr error) {
	if target == nil {
		return Result{}, downloadError(apperr.Local, "inspect download destination", errors.New("missing download target"))
	}
	committed := false
	defer func() {
		if closeErr := target.Close(); !committed && returnErr == nil && closeErr != nil {
			result = Result{}
			returnErr = downloadError(apperr.Local, "close download destination directory", closeErr)
		}
	}()

	metadata, err := d.API.FileMetadata(ctx, itemIdentity(item), item.Rev)
	if err != nil {
		return Result{}, classifyDownloadError("read download metadata", err)
	}
	if err := validateMetadata(item, metadata); err != nil {
		return Result{}, err
	}
	if progress != nil {
		progress.Started(metadata.Size)
	}

	saveName := metadata.Name
	if saveName == "" {
		saveName = item.Name
	}
	if saveName == "" {
		saveName = filepath.Base(displayPath)
	}
	stream, err := d.openStream(ctx, metadata, saveName)
	if err != nil {
		return Result{}, err
	}
	if stream.Body == nil {
		return Result{}, downloadError(apperr.Network, "open object download", errors.New("object response body is missing"))
	}
	defer stream.Body.Close()
	if stream.ContentLength >= 0 && stream.ContentLength != metadata.Size {
		return Result{}, integrityError("download content length differs from metadata")
	}

	temporary, err := target.CreateTemp()
	if err != nil {
		return Result{}, downloadError(apperr.Local, "create download temporary file", err)
	}
	defer func() {
		if closeErr := temporary.Close(); !committed && returnErr == nil && closeErr != nil {
			result = Result{}
			returnErr = downloadError(apperr.Local, "close download temporary file", closeErr)
		}
	}()
	if err := temporary.ValidateForWrite(); err != nil {
		return Result{}, downloadError(apperr.Local, "validate download temporary file", err)
	}

	checksum, err := copyExact(ctx, temporary.File(), stream.Body, metadata.Size, progress)
	if err != nil {
		return Result{}, err
	}
	if metadata.MD5 != "" && !strings.EqualFold(metadata.MD5, checksum) {
		return Result{}, integrityError("download checksum differs from metadata")
	}
	if err := temporary.Sync(); err != nil {
		return Result{}, downloadError(apperr.Local, "sync downloaded file", err)
	}
	if err := target.Install(temporary, metadata.Size); err != nil {
		return Result{}, downloadError(apperr.Local, "install downloaded file", err)
	}
	committed = true

	if progress != nil {
		progress.Finished()
	}
	return Result{LocalPath: displayPath, RemoteID: itemIdentity(metadata), Revision: metadata.Rev, Size: metadata.Size}, nil
}

func validateDownloadItem(item anyshare.Item) error {
	if itemIdentity(item) == "" || item.Type != "file" || item.Rev == "" || item.Size < 0 {
		return errors.New("remote path is not an exact file revision")
	}
	return nil
}

func (d *Downloader) openStream(ctx context.Context, item anyshare.Item, saveName string) (anyshare.DownloadStream, error) {
	for attempt := 0; attempt < 2; attempt++ {
		signed, err := d.API.AuthorizeDownload(ctx, itemIdentity(item), item.Rev, saveName)
		if err != nil {
			return anyshare.DownloadStream{}, classifyDownloadError("authorize download", err)
		}
		stream, err := d.API.GetSigned(ctx, signed)
		if err == nil {
			return stream, nil
		}
		if stream.Body != nil {
			_ = stream.Body.Close()
		}
		if attempt == 0 && anyshare.IsExpiredSignature(err) {
			continue
		}
		return anyshare.DownloadStream{}, classifyDownloadError("open object download", err)
	}
	panic("unreachable")
}

func validateMetadata(requested, current anyshare.Item) error {
	if itemIdentity(current) != itemIdentity(requested) || current.Type != "file" || current.Rev == "" || current.Rev != requested.Rev || current.Size != requested.Size {
		return integrityError("remote identity, revision, or size changed before download")
	}
	if current.MD5 != "" {
		decoded, err := hex.DecodeString(current.MD5)
		if err != nil || len(decoded) != md5.Size {
			return integrityError("remote download checksum is invalid")
		}
	}
	return nil
}

func copyExact(ctx context.Context, destination *os.File, source io.Reader, expected int64, progress Progress) (string, error) {
	hash := md5.New()
	buffer := make([]byte, downloadBufferSize)
	var written int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", downloadError(apperr.Interrupted, "download object", err)
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			emptyReads = 0
			if written+int64(n) > expected {
				return "", integrityError("download contains more bytes than metadata")
			}
			if err := writeAll(destination, buffer[:n]); err != nil {
				return "", downloadError(apperr.Local, "write downloaded file", err)
			}
			_, _ = hash.Write(buffer[:n])
			written += int64(n)
			if progress != nil {
				progress.Advanced(int64(n))
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return "", downloadError(apperr.Network, "download object", io.ErrNoProgress)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
					return "", downloadError(apperr.Interrupted, "download object", readErr)
				}
				return "", downloadError(apperr.Network, "download object", errors.New("object response interrupted"))
			}
			break
		}
	}
	if written != expected {
		return "", integrityError("download contains fewer bytes than metadata")
	}
	return strings.ToUpper(hex.EncodeToString(hash.Sum(nil))), nil
}

func writeAll(file *os.File, data []byte) error {
	for len(data) != 0 {
		n, err := file.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func itemIdentity(item anyshare.Item) string {
	if item.ID != "" {
		return item.ID
	}
	return item.DocID
}

func classifyDownloadError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return downloadError(apperr.Interrupted, op, err)
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) && appErr != nil {
		return apperr.Wrap(appErr.Category, op, "download failed", err)
	}
	if anyshare.IsExpiredSignature(err) {
		return downloadError(apperr.Remote, op, err)
	}
	return downloadError(apperr.Network, op, err)
}

func downloadError(category apperr.Category, op string, err error) error {
	return apperr.Wrap(category, op, "download failed", err)
}

func integrityError(message string) error {
	return apperr.Wrap(apperr.Integrity, "verify download", message, errors.New("download differs from remote metadata"))
}
