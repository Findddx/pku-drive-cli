package remote

import (
	"context"
	"errors"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

// WalkFiles returns every file at or below root (the root itself when it is a
// file), each entry carrying its visible path. The order is deterministic:
// depth-first with same-level entries in lexical name order. The virtual root
// "/" is rejected; file/directory type anomalies and files lacking an ID or
// revision are Remote errors (downloads validate Rev, see
// downloader.validateDownloadItem). Two same-named entries within one listing
// (a file/directory name collision) are a Remote conflict error, matching the
// resolver exactChild discipline; the client never guesses a deduplication.
// The implementation reuses the package resolver: every directory is listed
// exactly once through childrenAt (pagination merged by the Documents client),
// and a first-level entry-document-library root goes through the
// isDirectory(item, library=true) special case.
func (s *Service) WalkFiles(ctx context.Context, root string) ([]anyshare.Item, error) {
	normalized, err := Normalize(root)
	if err != nil {
		return nil, err
	}
	if normalized == "/" {
		return nil, apperr.Wrap(apperr.Usage, "walk files", "remote root is virtual", errors.New("cannot walk the virtual root"))
	}
	r := s.resolver()
	if err := r.ready(); err != nil {
		return nil, err
	}
	item, err := r.resolve(ctx, normalized)
	if err != nil {
		return nil, err
	}
	files := make([]anyshare.Item, 0)
	if err := s.walkCollect(ctx, r, item, len(pathParts(normalized)) == 1, &files); err != nil {
		return nil, err
	}
	return files, nil
}

// walkCollect appends the files at or below item to files in depth-first
// lexical order. library marks a first-level entry document library, whose
// type is the library type rather than "directory".
func (s *Service) walkCollect(ctx context.Context, r *resolver, item anyshare.Item, library bool, files *[]anyshare.Item) error {
	if item.Type == "file" {
		if itemID(item) == "" || item.Rev == "" || item.Size < 0 {
			return remoteError("walk files", "remote file identity is incomplete", nil)
		}
		*files = append(*files, item)
		return nil
	}
	if !isDirectory(item, library) {
		return remoteError("walk files", "remote object type is not a file or directory", nil)
	}
	children, err := r.childrenAt(ctx, itemID(item))
	if err != nil {
		return err
	}
	if duplicateChildName(children) {
		return remoteConflict("walk files", item.Path)
	}
	for _, child := range sortedCopy(children) {
		child = visibleItem(child, item.Path+"/"+child.Name)
		if err := s.walkCollect(ctx, r, child, false, files); err != nil {
			return err
		}
	}
	return nil
}

// duplicateChildName reports whether one listing contains two same-named
// entries, mirroring exactChild's conflict discipline for tree walks.
func duplicateChildName(children []anyshare.Item) bool {
	seen := make(map[string]struct{}, len(children))
	for _, child := range children {
		if _, exists := seen[child.Name]; exists {
			return true
		}
		seen[child.Name] = struct{}{}
	}
	return false
}
