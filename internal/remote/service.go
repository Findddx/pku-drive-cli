package remote

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

var errNotFound = errors.New("remote path not found")

// Documents is the document API required for visible path operations.
type Documents interface {
	EntryDocLibs(ctx context.Context) ([]anyshare.Item, error)
	ListFolder(ctx context.Context, id string) ([]anyshare.Item, error)
	CreateDir(ctx context.Context, parentID, name string) (anyshare.Item, error)
	DeleteFile(ctx context.Context, id string) (anyshare.DeleteResult, error)
	DeleteDir(ctx context.Context, id string) (anyshare.DeleteResult, error)
}

// Service resolves AnyShare display paths. It intentionally keeps no remote
// object IDs or folder listings between public operations.
type Service struct {
	documents Documents
}

// UploadTarget identifies the remote parent and name selected for an upload.
type UploadTarget struct {
	RemotePath string
	ParentID   string
	Name       string
	Existing   *anyshare.Item
}

// DeleteResult identifies the exact resolved object and whether AnyShare
// deleted it immediately or accepted it for review.
type DeleteResult struct {
	Item   anyshare.Item
	Status anyshare.DeleteStatus
}

// NewService constructs a remote path service.
func NewService(documents Documents) *Service {
	return &Service{documents: documents}
}

// Resolve returns the exact non-root item at path.
func (s *Service) Resolve(ctx context.Context, path string) (anyshare.Item, error) {
	normalized, err := Normalize(path)
	if err != nil {
		return anyshare.Item{}, err
	}
	if normalized == "/" {
		return anyshare.Item{}, remoteError("resolve", "remote root is virtual", nil)
	}
	return s.resolver().resolve(ctx, normalized)
}

// List returns deterministic entries at path. The virtual root contains entry
// document libraries; all other paths must identify a directory.
func (s *Service) List(ctx context.Context, path string) ([]anyshare.Item, error) {
	normalized, err := Normalize(path)
	if err != nil {
		return nil, err
	}
	r := s.resolver()
	if err := r.ready(); err != nil {
		return nil, err
	}
	if normalized == "/" {
		return r.librariesAtRoot(ctx)
	}
	item, err := r.resolve(ctx, normalized)
	if err != nil {
		return nil, err
	}
	if !isDirectory(item, len(pathParts(normalized)) == 1) {
		return nil, remoteError("list", "remote path is not a directory", nil)
	}
	children, err := r.childrenAt(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	return visibleChildren(children, normalized), nil
}

// Mkdir creates path's missing directory components only when parents is set.
// A directory that already exists is returned unchanged.
func (s *Service) Mkdir(ctx context.Context, path string, parents bool) (anyshare.Item, error) {
	normalized, err := Normalize(path)
	if err != nil {
		return anyshare.Item{}, err
	}
	if normalized == "/" {
		return anyshare.Item{}, remoteError("mkdir", "remote root is virtual", nil)
	}
	r := s.resolver()
	if err := r.ready(); err != nil {
		return anyshare.Item{}, err
	}
	parts := pathParts(normalized)
	current, err := r.library(ctx, parts[0])
	if err != nil {
		return anyshare.Item{}, err
	}
	if len(parts) == 1 {
		return current, nil
	}

	currentPath := "/" + parts[0]
	for i := 1; i < len(parts); i++ {
		if !isDirectory(current, i == 1) {
			return anyshare.Item{}, remoteError("mkdir", "remote ancestor is not a directory", nil)
		}
		name := parts[i]
		childPath := currentPath + "/" + name
		children, err := r.childrenAt(ctx, current.ID)
		if err != nil {
			return anyshare.Item{}, err
		}
		child, found, err := exactChild(children, name)
		if err != nil {
			return anyshare.Item{}, err
		}
		if found {
			if child.Type != "directory" {
				return anyshare.Item{}, remoteConflict("mkdir", childPath)
			}
			current = visibleItem(child, childPath)
			currentPath = childPath
			continue
		}
		if !parents && i != len(parts)-1 {
			return anyshare.Item{}, notFound(childPath)
		}

		created, createErr := s.documents.CreateDir(ctx, current.ID, name)
		if createErr == nil {
			created.Name = name
			created.Type = "directory"
			current = visibleItem(created, childPath)
			currentPath = childPath
			continue
		}
		if !ambiguousCreateError(createErr) {
			return anyshare.Item{}, createErr
		}
		// CreateDir is non-idempotent. A relist reconciles a lost response or
		// concurrent creator, rather than issuing another request.
		r.invalidate(current.ID)
		children, listErr := r.childrenAt(ctx, current.ID)
		if listErr != nil {
			return anyshare.Item{}, createErr
		}
		child, found, reconcileErr := exactChild(children, name)
		if reconcileErr != nil {
			return anyshare.Item{}, reconcileErr
		}
		if found {
			if child.Type != "directory" {
				return anyshare.Item{}, remoteConflict("mkdir", childPath)
			}
			current = visibleItem(child, childPath)
			currentPath = childPath
			continue
		}
		return anyshare.Item{}, createErr
	}
	return current, nil
}

// Delete resolves path once and deletes that exact object ID. Files do not
// require recursive. Every directory requires an explicit recursive request
// because AnyShare's directory endpoint deletes the whole tree and offers no
// atomic empty-directory-only operation.
func (s *Service) Delete(ctx context.Context, path string, recursive bool) (DeleteResult, error) {
	normalized, err := Normalize(path)
	if err != nil {
		return DeleteResult{}, err
	}
	if normalized == "/" {
		return DeleteResult{}, remoteError("delete", "remote root is virtual", nil)
	}
	parts := pathParts(normalized)
	if len(parts) == 1 {
		return DeleteResult{}, remoteError("delete", "entry document libraries cannot be deleted", nil)
	}

	r := s.resolver()
	if err := r.ready(); err != nil {
		return DeleteResult{}, err
	}
	item, err := r.resolve(ctx, normalized)
	if err != nil {
		return DeleteResult{}, err
	}
	targetID := itemID(item)
	if targetID == "" {
		return DeleteResult{}, remoteError("delete", "remote object has no document ID", nil)
	}
	item.ID = targetID

	var apiResult anyshare.DeleteResult
	switch item.Type {
	case "file":
		apiResult, err = s.documents.DeleteFile(ctx, targetID)
	case "directory":
		if !recursive {
			return DeleteResult{}, remoteError("delete", "remote path is a directory; recursive deletion is required", nil)
		}
		apiResult, err = s.documents.DeleteDir(ctx, targetID)
	default:
		return DeleteResult{}, remoteError("delete", "remote object type cannot be deleted", nil)
	}
	if err != nil {
		return DeleteResult{}, err
	}
	if apiResult.Status != anyshare.DeleteStatusDeleted && apiResult.Status != anyshare.DeleteStatusPendingReview {
		return DeleteResult{}, remoteError("delete", "invalid delete status", nil)
	}
	return DeleteResult{Item: item, Status: apiResult.Status}, nil
}

func itemID(item anyshare.Item) string {
	if item.ID != "" {
		return item.ID
	}
	return item.DocID
}

// ResolveUploadTarget chooses the destination name without uploading bytes.
func (s *Service) ResolveUploadTarget(ctx context.Context, localBase, remotePath string) (UploadTarget, error) {
	if err := validBaseName(localBase); err != nil {
		return UploadTarget{}, err
	}
	trailingSlash := strings.HasSuffix(remotePath, "/")
	normalized, err := Normalize(remotePath)
	if err != nil {
		return UploadTarget{}, err
	}
	if normalized == "/" {
		return UploadTarget{}, remoteError("resolve upload target", "remote root is virtual", nil)
	}
	r := s.resolver()
	if err := r.ready(); err != nil {
		return UploadTarget{}, err
	}
	parts := pathParts(normalized)
	item, resolveErr := r.resolve(ctx, normalized)
	if resolveErr == nil {
		if trailingSlash || isDirectory(item, len(parts) == 1) {
			if !isDirectory(item, len(parts) == 1) {
				return UploadTarget{}, remoteConflict("resolve upload target", normalized)
			}
			return r.targetInDirectory(ctx, item, normalized, localBase)
		}
		if item.Type != "file" {
			return UploadTarget{}, remoteConflict("resolve upload target", normalized)
		}
		parent, err := r.parent(ctx, parts)
		if err != nil {
			return UploadTarget{}, err
		}
		item = visibleItem(item, normalized)
		return UploadTarget{RemotePath: normalized, ParentID: parent.ID, Name: item.Name, Existing: &item}, nil
	}
	if !errors.Is(resolveErr, errNotFound) {
		return UploadTarget{}, resolveErr
	}
	if trailingSlash {
		return UploadTarget{}, resolveErr
	}
	parent, err := r.parent(ctx, parts)
	if err != nil {
		return UploadTarget{}, err
	}
	return UploadTarget{RemotePath: normalized, ParentID: parent.ID, Name: parts[len(parts)-1]}, nil
}

func (s *Service) resolver() *resolver {
	return &resolver{documents: s.documents, children: make(map[string][]anyshare.Item)}
}

type resolver struct {
	documents Documents
	loaded    bool
	libraries []anyshare.Item
	children  map[string][]anyshare.Item
}

func (r *resolver) ready() error {
	if r.documents == nil {
		return apperr.Wrap(apperr.Local, "remote", "documents client is not configured", errors.New("missing documents dependency"))
	}
	return nil
}

func (r *resolver) librariesAtRoot(ctx context.Context) ([]anyshare.Item, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if !r.loaded {
		items, err := r.documents.EntryDocLibs(ctx)
		if err != nil {
			return nil, err
		}
		r.libraries = sortedCopy(items)
		r.loaded = true
	}
	return visibleLibraries(r.libraries), nil
}

func (r *resolver) library(ctx context.Context, name string) (anyshare.Item, error) {
	items, err := r.librariesAtRoot(ctx)
	if err != nil {
		return anyshare.Item{}, err
	}
	item, found, err := exactChild(items, name)
	if err != nil {
		return anyshare.Item{}, err
	}
	if !found {
		return anyshare.Item{}, notFound("/" + name)
	}
	return visibleItem(item, "/"+name), nil
}

func (r *resolver) childrenAt(ctx context.Context, id string) ([]anyshare.Item, error) {
	if children, ok := r.children[id]; ok {
		return children, nil
	}
	items, err := r.documents.ListFolder(ctx, id)
	if err != nil {
		return nil, err
	}
	items = sortedCopy(items)
	r.children[id] = items
	return items, nil
}

func (r *resolver) invalidate(id string) {
	delete(r.children, id)
}

func (r *resolver) resolve(ctx context.Context, normalized string) (anyshare.Item, error) {
	parts := pathParts(normalized)
	if len(parts) == 0 {
		return anyshare.Item{}, remoteError("resolve", "remote root is virtual", nil)
	}
	current, err := r.library(ctx, parts[0])
	if err != nil {
		return anyshare.Item{}, err
	}
	currentPath := "/" + parts[0]
	for i := 1; i < len(parts); i++ {
		if !isDirectory(current, i == 1) {
			return anyshare.Item{}, remoteError("resolve", "remote ancestor is not a directory", nil)
		}
		children, err := r.childrenAt(ctx, current.ID)
		if err != nil {
			return anyshare.Item{}, err
		}
		child, found, err := exactChild(children, parts[i])
		if err != nil {
			return anyshare.Item{}, err
		}
		currentPath += "/" + parts[i]
		if !found {
			return anyshare.Item{}, notFound(currentPath)
		}
		current = visibleItem(child, currentPath)
	}
	return current, nil
}

func (r *resolver) parent(ctx context.Context, parts []string) (anyshare.Item, error) {
	if len(parts) < 2 {
		return anyshare.Item{}, remoteError("resolve upload target", "remote root cannot contain files", nil)
	}
	parentPath := "/" + strings.Join(parts[:len(parts)-1], "/")
	parent, err := r.resolve(ctx, parentPath)
	if err != nil {
		return anyshare.Item{}, err
	}
	if !isDirectory(parent, len(parts) == 2) {
		return anyshare.Item{}, remoteError("resolve upload target", "remote parent is not a directory", nil)
	}
	return parent, nil
}

func (r *resolver) targetInDirectory(ctx context.Context, parent anyshare.Item, parentPath, name string) (UploadTarget, error) {
	children, err := r.childrenAt(ctx, parent.ID)
	if err != nil {
		return UploadTarget{}, err
	}
	remotePath := parentPath + "/" + name
	child, found, err := exactChild(children, name)
	if err != nil {
		return UploadTarget{}, err
	}
	if !found {
		return UploadTarget{RemotePath: remotePath, ParentID: parent.ID, Name: name}, nil
	}
	child = visibleItem(child, remotePath)
	if child.Type != "file" {
		return UploadTarget{}, remoteConflict("resolve upload target", remotePath)
	}
	return UploadTarget{RemotePath: remotePath, ParentID: parent.ID, Name: name, Existing: &child}, nil
}

func pathParts(normalized string) []string {
	if normalized == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(normalized, "/"), "/")
}

func sortedCopy(items []anyshare.Item) []anyshare.Item {
	result := append([]anyshare.Item(nil), items...)
	sort.SliceStable(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func visibleLibraries(items []anyshare.Item) []anyshare.Item {
	result := sortedCopy(items)
	for i := range result {
		result[i] = visibleItem(result[i], "/"+result[i].Name)
	}
	return result
}

func visibleChildren(items []anyshare.Item, parentPath string) []anyshare.Item {
	result := sortedCopy(items)
	for i := range result {
		result[i] = visibleItem(result[i], parentPath+"/"+result[i].Name)
	}
	return result
}

func visibleItem(item anyshare.Item, path string) anyshare.Item {
	item.Path = path
	return item
}

func exactChild(items []anyshare.Item, name string) (anyshare.Item, bool, error) {
	var found anyshare.Item
	count := 0
	for _, item := range items {
		if item.Name == name {
			found = item
			count++
		}
	}
	if count > 1 {
		return anyshare.Item{}, false, remoteConflict("resolve", name)
	}
	return found, count == 1, nil
}

func isDirectory(item anyshare.Item, library bool) bool {
	return item.Type == "directory" || library
}

func validBaseName(name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") || strings.IndexByte(name, 0) >= 0 {
		return invalidPath("local basename is invalid")
	}
	return nil
}

func ambiguousCreateError(err error) bool {
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr == nil {
		return true
	}
	switch appErr.Category {
	case apperr.Network, apperr.Remote:
		return true
	case apperr.Auth, apperr.Interrupted, apperr.Local, apperr.Usage, apperr.Integrity:
		return false
	default:
		return true
	}
}

func notFound(path string) error {
	return apperr.Wrap(apperr.Remote, "remote path", "not found: "+path, errNotFound)
}

func remoteConflict(op, path string) error {
	return remoteError(op, "remote conflict: "+path, nil)
}

func remoteError(op, message string, err error) error {
	if err == nil {
		err = errors.New("remote operation failed")
	}
	return apperr.Wrap(apperr.Remote, op, message, err)
}
