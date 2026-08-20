package sharelink

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/download"
)

// Downloaded pairs a selected shared entry with its verified local result.
type Downloaded struct {
	Entry  Entry
	Result download.Result
}

// Download resolves and preflights the complete selection before starting any
// transfer, then downloads files in the caller's stable input order. A batch
// is not transactional, while every individual file is installed atomically.
func (s *Session) Download(ctx context.Context, relativeFiles []string, localDir string, overwrite bool, progress download.Progress) (results []Downloaded, returnErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(relativeFiles) == 0 {
		return nil, apperr.Wrap(apperr.Usage, "share download", "no files selected", errors.New("empty selection"))
	}
	entries := make([]Entry, len(relativeFiles))
	seen := make(map[string]struct{}, len(relativeFiles))
	var total int64
	for index, relative := range relativeFiles {
		if _, exists := seen[relative]; exists {
			return nil, apperr.Wrap(apperr.Usage, "share download", "duplicate file selection", errors.New("duplicate relative path"))
		}
		seen[relative] = struct{}{}
		entry, err := s.Resolve(ctx, relative)
		if err != nil {
			return nil, err
		}
		if entry.Item.Rev == "" || entry.Size < 0 || itemID(entry.Item) == "" {
			return nil, invalidRemoteError("shared file identity is incomplete")
		}
		if entry.Size > math.MaxInt64-total {
			return nil, invalidRemoteError("shared selection size is invalid")
		}
		total += entry.Size
		entries[index] = entry
	}

	root, err := download.OpenDestinationRoot(localDir)
	if err != nil {
		return nil, apperr.Wrap(apperr.Local, "share download", "open destination directory", err)
	}
	defer func() {
		// A cleanup failure must not turn already published, verified files into
		// an apparent batch failure. Before the first publication, retain it.
		if closeErr := root.Close(); returnErr == nil && len(results) == 0 && closeErr != nil {
			results = nil
			returnErr = apperr.Wrap(apperr.Local, "share download", "close destination directory", closeErr)
		}
	}()
	for _, entry := range entries {
		if err := root.Preflight(entry.Path, overwrite); err != nil {
			return nil, apperr.Wrap(apperr.Local, "share download", "preflight destination", err)
		}
	}

	if progress != nil {
		progress.Started(total)
	}
	fileProgress := download.Progress(nil)
	if progress != nil {
		fileProgress = advancedOnlyProgress{parent: progress}
	}
	downloader := download.NewDownloader(s.api)
	results = make([]Downloaded, 0, len(entries))
	for _, entry := range entries {
		result, err := downloader.GetAt(ctx, entry.Item, root, entry.Path, overwrite, fileProgress)
		if err != nil {
			classified := classifyAPIError(err)
			if len(results) > 0 {
				return results, partialDownloadError(classified, len(results))
			}
			return results, classified
		}
		results = append(results, Downloaded{Entry: entry, Result: result})
	}
	if progress != nil {
		progress.Finished()
	}
	return results, nil
}

func partialDownloadError(cause error, completed int) error {
	category := apperr.Network
	var appErr *apperr.Error
	if errors.As(cause, &appErr) && appErr != nil {
		category = appErr.Category
	}
	message := fmt.Sprintf("download stopped after %d completed file(s); completed files remain in destination", completed)
	return apperr.Wrap(category, "share download", message, cause)
}

type advancedOnlyProgress struct{ parent download.Progress }

func (p advancedOnlyProgress) Started(int64) {}

func (p advancedOnlyProgress) Advanced(delta int64) {
	if p.parent != nil {
		p.parent.Advanced(delta)
	}
}

func (p advancedOnlyProgress) Finished() {}
