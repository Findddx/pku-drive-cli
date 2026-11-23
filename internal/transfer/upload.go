package transfer

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/remote"
	"github.com/Findddx/pku-drive-cli/internal/upload"
)

// RemoteDirs is the remote directory face required for recursive uploads
// (satisfied by *remote.Service).
type RemoteDirs interface {
	Mkdir(ctx context.Context, path string, parents bool) (anyshare.Item, error)
	List(ctx context.Context, path string) ([]anyshare.Item, error)
}

// BatchUploader is the per-file upload face (satisfied by *upload.Uploader).
type BatchUploader interface {
	Put(ctx context.Context, localPath string, target remote.UploadTarget,
		overwrite bool, progress upload.Progress) (upload.Result, error)
}

// UploadTree uploads the whole local tree (or single file) at localDir below
// remotePath. Phase one serially creates directories in ascending depth order
// via Mkdir(parents=true) and builds each directory's existing-children map
// from one List; phase two uploads files through a worker pool. The report's
// Completed and Skipped slices are sorted by relative path and never nil.
func UploadTree(ctx context.Context, dirs RemoteDirs, uploader BatchUploader,
	localDir, remotePath string, opts Options, progress upload.Progress) (UploadReport, error) {
	normalized, err := remote.Normalize(remotePath)
	if err != nil {
		return UploadReport{}, err
	}
	if normalized == "/" {
		return UploadReport{}, apperr.Wrap(apperr.Usage, "upload tree", "remote root is virtual", errors.New("refusing to upload into the virtual root"))
	}
	if ctx == nil {
		ctx = context.Background()
	}

	files, skipped, err := walkLocalFiles(localDir)
	if err != nil {
		return UploadReport{}, err
	}
	if len(files) == 0 {
		return UploadReport{Completed: make([]CompletedUpload, 0), Skipped: skipped}, nil
	}

	// Phase one: directories in ascending depth, then lexical order. Missing
	// parents are created by Mkdir's reconciliation discipline; empty local
	// directories produce no entry here and are never created remotely.
	directorySet := make(map[string]struct{})
	for _, file := range files {
		dir := parentDirectory(file.Relative)
		for dir != "" {
			if _, seen := directorySet[dir]; seen {
				break
			}
			directorySet[dir] = struct{}{}
			dir = parentDirectory(dir)
		}
	}
	directories := make([]string, 0, len(directorySet))
	for dir := range directorySet {
		directories = append(directories, dir)
	}
	sort.SliceStable(directories, func(i, j int) bool {
		depthI, depthJ := strings.Count(directories[i], "/"), strings.Count(directories[j], "/")
		if depthI != depthJ {
			return depthI < depthJ
		}
		return directories[i] < directories[j]
	})

	type directoryPlan struct {
		remotePath string
		parentID   string
		children   map[string]anyshare.Item
	}
	plans := make(map[string]directoryPlan, len(directories)+1)

	mkdirItem, err := dirs.Mkdir(ctx, normalized, true)
	if err != nil {
		return UploadReport{}, err
	}
	rootChildren, err := dirs.List(ctx, normalized)
	if err != nil {
		return UploadReport{}, err
	}
	plans[""] = directoryPlan{remotePath: normalized, parentID: itemIdentityOf(mkdirItem), children: childMap(rootChildren)}

	for _, dir := range directories {
		remoteDir := normalized + "/" + dir
		mkdirItem, err := dirs.Mkdir(ctx, remoteDir, true)
		if err != nil {
			return UploadReport{}, err
		}
		children, err := dirs.List(ctx, remoteDir)
		if err != nil {
			return UploadReport{}, err
		}
		plans[dir] = directoryPlan{remotePath: remoteDir, parentID: itemIdentityOf(mkdirItem), children: childMap(children)}
	}

	// Phase two: per-file worker pool. Targets are constructed from the
	// directory listing map, avoiding a per-file ResolveUploadTarget walk.
	var mu sync.Mutex
	report := UploadReport{Completed: make([]CompletedUpload, 0), Skipped: skipped}
	poolErr := runPool(ctx, opts.Jobs, makeTasks(len(files), func(index int) func(context.Context) error {
		return func(taskCtx context.Context) error {
			file := files[index]
			dir := parentDirectory(file.Relative)
			plan := plans[dir]
			name := baseName(file.Relative)
			existing, found := plan.children[name]
			if found {
				if existing.Type == "directory" {
					return apperr.Wrap(apperr.Remote, "upload tree", "remote conflict: "+plan.remotePath+"/"+name, errors.New("a same-named remote directory exists"))
				}
				if !opts.Overwrite {
					mu.Lock()
					report.Skipped = append(report.Skipped, Skipped{Path: file.Relative, Reason: SkipExists})
					mu.Unlock()
					return nil
				}
			}
			target := remote.UploadTarget{
				RemotePath: plan.remotePath + "/" + name,
				ParentID:   plan.parentID,
				Name:       name,
				Existing:   existingOrNil(existing, found),
			}
			result, err := uploader.Put(taskCtx, file.Absolute, target, opts.Overwrite, advancedOnlyUpload{parent: progress})
			if err != nil {
				return err
			}
			mu.Lock()
			report.Completed = append(report.Completed, CompletedUpload{Relative: file.Relative, Result: result})
			mu.Unlock()
			return nil
		}
	}))
	if poolErr != nil {
		if errors.Is(poolErr, context.Canceled) || errors.Is(poolErr, context.DeadlineExceeded) {
			poolErr = apperr.Wrap(apperr.Interrupted, "upload tree", "interrupted", poolErr)
		}
		sortUploadReport(&report)
		if len(report.Completed) > 0 {
			poolErr = partialTreeError(poolErr, len(report.Completed), "upload", "remote")
		}
		return report, poolErr
	}
	if progress != nil {
		progress.Finished()
	}
	sortUploadReport(&report)
	return report, nil
}

func sortUploadReport(report *UploadReport) {
	sort.SliceStable(report.Completed, func(i, j int) bool { return report.Completed[i].Relative < report.Completed[j].Relative })
	sortSkipped(report.Skipped)
}

func parentDirectory(relative string) string {
	if index := strings.LastIndex(relative, "/"); index >= 0 {
		return relative[:index]
	}
	return ""
}

func baseName(relative string) string {
	if index := strings.LastIndex(relative, "/"); index >= 0 {
		return relative[index+1:]
	}
	return relative
}

func childMap(children []anyshare.Item) map[string]anyshare.Item {
	result := make(map[string]anyshare.Item, len(children))
	for _, child := range children {
		result[child.Name] = child
	}
	return result
}

func itemIdentityOf(item anyshare.Item) string {
	if item.ID != "" {
		return item.ID
	}
	return item.DocID
}

func existingOrNil(item anyshare.Item, found bool) *anyshare.Item {
	if !found {
		return nil
	}
	return &item
}
