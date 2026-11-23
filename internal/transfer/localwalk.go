package transfer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

// localFile is one regular file collected from the local tree.
type localFile struct {
	Relative string
	Absolute string
}

// localEntryKind classifies one local directory entry.
type localEntryKind int

const (
	localEntryFile localEntryKind = iota
	localEntryDirectory
	localEntrySymlink
	localEntryUnsupported
)

// classifyLocalEntry is a pure function deciding whether a directory entry is
// a file, a directory, a symlink, or an unsupported non-regular entry. It is
// kept separate so device/fifo branches can be unit tested without privileged
// mknod.
func classifyLocalEntry(typ fs.FileMode) (kind localEntryKind, err error) {
	switch {
	case typ&fs.ModeSymlink != 0:
		return localEntrySymlink, nil
	case typ.IsRegular():
		return localEntryFile, nil
	case typ.IsDir():
		return localEntryDirectory, nil
	case typ&fs.ModeSocket != 0 || typ&fs.ModeNamedPipe != 0 || typ&fs.ModeDevice != 0:
		return localEntryUnsupported, localWalkError("unsupported local entry (socket, pipe, or device)")
	default:
		return localEntryUnsupported, localWalkError("unsupported local entry type")
	}
}

func localWalkError(message string) error {
	return apperr.Wrap(apperr.Local, "walk local tree", message, errors.New("local tree cannot be transferred"))
}

// walkLocalFiles depth-first collects every regular file below root; symlinks
// are recorded in skipped without being followed (filepath.WalkDir never
// follows them); fifo/socket/device entries return a Local error; empty
// directories naturally produce no entries. A single-file root returns that
// one entry with Relative = basename. A missing root is a Local error, and a
// root that is itself a symlink is also a Local error: the root is the path
// the user chose directly, unlike incidental symlinks discovered during the
// walk, so it is refused rather than resolved, following the project-wide
// O_NOFOLLOW posture.
func walkLocalFiles(root string) (files []localFile, skipped []Skipped, err error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.Local, "walk local tree", "local path does not exist or cannot be inspected", err)
	}
	kind, classifyErr := classifyLocalEntry(info.Mode())
	if classifyErr != nil {
		return nil, nil, classifyErr
	}
	if kind == localEntrySymlink {
		return nil, nil, localWalkError("local path is a symbolic link; pass the real path")
	}
	if kind == localEntryFile {
		abs, absErr := filepath.Abs(root)
		if absErr != nil {
			return nil, nil, apperr.Wrap(apperr.Local, "walk local tree", "resolve local path", absErr)
		}
		return []localFile{{Relative: filepath.Base(abs), Absolute: abs}}, nil, nil
	}

	files = make([]localFile, 0)
	skipped = make([]Skipped, 0)
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		entryKind, classifyErr := classifyLocalEntry(entry.Type())
		if classifyErr != nil {
			return classifyErr
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return apperr.Wrap(apperr.Local, "walk local tree", "compute relative path", relErr)
		}
		switch entryKind {
		case localEntrySymlink:
			skipped = append(skipped, Skipped{Path: filepath.ToSlash(relative), Reason: SkipSymlink})
			return nil
		case localEntryDirectory:
			return nil
		case localEntryFile:
			abs, absErr := filepath.Abs(path)
			if absErr != nil {
				return apperr.Wrap(apperr.Local, "walk local tree", "resolve local path", absErr)
			}
			files = append(files, localFile{Relative: filepath.ToSlash(relative), Absolute: abs})
			return nil
		default:
			return localWalkError("unsupported local entry (socket, pipe, or device)")
		}
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	sortLocalFiles(files)
	sortSkipped(skipped)
	return files, skipped, nil
}

func sortLocalFiles(files []localFile) {
	sort.SliceStable(files, func(i, j int) bool { return files[i].Relative < files[j].Relative })
}

func sortSkipped(skipped []Skipped) {
	sort.SliceStable(skipped, func(i, j int) bool { return skipped[i].Path < skipped[j].Path })
}
