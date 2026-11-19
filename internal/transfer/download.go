package transfer

import (
	"context"
	"errors"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/download"
	"github.com/Findddx/pku-drive-cli/internal/remote"
)

// RemoteTree is the remote traversal face required for recursive downloads
// (satisfied by *remote.Service).
type RemoteTree interface {
	WalkFiles(ctx context.Context, root string) ([]anyshare.Item, error)
}

// BatchDownloader is the per-file download face (satisfied by
// *download.Downloader).
type BatchDownloader interface {
	GetAt(ctx context.Context, item anyshare.Item, root *download.DestinationRoot,
		relativePath string, overwrite bool, progress download.Progress) (download.Result, error)
}

// DownloadTree downloads the whole file tree at remotePath below the existing
// localDir. The first file error stops the batch and returns the completed
// portion with that first error (apperr-classified). Local relative paths are
// the WalkFiles entry paths with the "<root>/" prefix removed. The report's
// Completed and Skipped slices are sorted by relative path and never nil.
func DownloadTree(ctx context.Context, tree RemoteTree, downloader BatchDownloader,
	remotePath, localDir string, opts Options, progress download.Progress) (DownloadReport, error) {
	normalized, err := remote.Normalize(remotePath)
	if err != nil {
		return DownloadReport{}, err
	}
	if normalized == "/" {
		return DownloadReport{}, apperr.Wrap(apperr.Usage, "download tree", "remote root is virtual", errors.New("refusing to walk the entire document store"))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	items, err := tree.WalkFiles(ctx, normalized)
	if err != nil {
		return DownloadReport{}, err
	}

	root, err := download.OpenDestinationRoot(localDir)
	if err != nil {
		return DownloadReport{}, apperr.Wrap(apperr.Local, "download tree", "open destination directory", err)
	}
	defer func() { _ = root.Close() }()

	prefix := normalized + "/"
	var total int64
	tasks := make([]struct {
		item     anyshare.Item
		relative string
	}, 0, len(items))
	for _, item := range items {
		relative := strings.TrimPrefix(item.Path, prefix)
		if item.Path == normalized || relative == item.Path {
			relative = path.Base(item.Path)
		}
		tasks = append(tasks, struct {
			item     anyshare.Item
			relative string
		}{item: item, relative: relative})
		if item.Size > math.MaxInt64-total {
			return DownloadReport{}, apperr.Wrap(apperr.Remote, "download tree", "total transfer size is invalid", errors.New("size overflow"))
		}
		total += item.Size
	}

	if progress != nil {
		progress.Started(total)
	}
	fileProgress := download.Progress(nil)
	if progress != nil {
		fileProgress = advancedOnlyDownload{parent: progress}
	}

	var mu sync.Mutex
	report := DownloadReport{Completed: make([]CompletedDownload, 0), Skipped: make([]Skipped, 0)}
	taskFuncs := make([]func(context.Context) error, len(tasks))
	for index := range tasks {
		taskFuncs[index] = func(taskCtx context.Context) error {
			task := tasks[index]
			if err := root.Preflight(task.relative, opts.Overwrite); err != nil {
				if !opts.Overwrite && errors.Is(err, download.ErrDestinationExists) {
					mu.Lock()
					report.Skipped = append(report.Skipped, Skipped{Path: task.relative, Reason: SkipExists})
					mu.Unlock()
					return nil
				}
				return apperr.Wrap(apperr.Local, "download tree", "preflight destination", err)
			}
			result, err := downloader.GetAt(taskCtx, task.item, root, task.relative, opts.Overwrite, fileProgress)
			if err != nil {
				return err
			}
			mu.Lock()
			report.Completed = append(report.Completed, CompletedDownload{Relative: task.relative, RemotePath: task.item.Path, Result: result})
			mu.Unlock()
			return nil
		}
	}
	poolErr := runPool(ctx, opts.Jobs, taskFuncs)
	if poolErr != nil {
		if errors.Is(poolErr, context.Canceled) || errors.Is(poolErr, context.DeadlineExceeded) {
			poolErr = apperr.Wrap(apperr.Interrupted, "download tree", "interrupted", poolErr)
		}
		sortReport(&report)
		if len(report.Completed) > 0 {
			poolErr = partialTreeError(poolErr, len(report.Completed), "download", "destination")
		}
		return report, poolErr
	}
	if progress != nil {
		progress.Finished()
	}
	sortReport(&report)
	return report, nil
}

// makeTasks adapts an index closure into a task slice for runPool.
func makeTasks(count int, makeTask func(index int) func(context.Context) error) []func(context.Context) error {
	result := make([]func(context.Context) error, count)
	for index := 0; index < count; index++ {
		result[index] = makeTask(index)
	}
	return result
}

func sortReport(report *DownloadReport) {
	sort.SliceStable(report.Completed, func(i, j int) bool { return report.Completed[i].Relative < report.Completed[j].Relative })
	sortSkipped(report.Skipped)
}

// partialTreeError preserves the first error's category while reporting how
// many files completed before the stop, following the sharelink
// partialDownloadError message style.
func partialTreeError(cause error, completed int, verb, where string) error {
	category := apperr.Network
	var appErr *apperr.Error
	if errors.As(cause, &appErr) && appErr != nil {
		category = appErr.Category
	}
	message := verb + " stopped after " + strconv.Itoa(completed) + " completed file(s); completed files remain in " + where
	return apperr.Wrap(category, verb+" tree", message, cause)
}
