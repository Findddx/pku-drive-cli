package remote

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

func TestListRootAndFolderUseVisibleNamesAndDeterministicOrder(t *testing.T) {
	docs := newFakeDocuments(
		[]anyshare.Item{
			library("z", "Zulu", "shared_doc_lib"),
			library("a", "Alpha", "user_doc_lib"),
		},
		map[string][]anyshare.Item{
			"a": {file("f", "z.txt"), directory("d", "a-dir")},
		},
	)
	svc := NewService(docs)

	root, err := svc.List(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := itemNames(root), []string{"Alpha", "Zulu"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("root names = %v, want %v", got, want)
	}
	if root[0].Path != "/Alpha" || root[0].Type != "user_doc_lib" || root[0].ID != "a" {
		t.Fatalf("root item = %#v", root[0])
	}

	entries, err := svc.List(context.Background(), "/Alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := itemNames(entries), []string{"a-dir", "z.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("folder names = %v, want %v", got, want)
	}
	if entries[0].Path != "/Alpha/a-dir" || entries[0].Type != "directory" || entries[1].ID != "f" {
		t.Fatalf("entries = %#v", entries)
	}
	if got := docs.listCount("a"); got != 1 {
		t.Fatalf("ListFolder(a) calls = %d, want 1", got)
	}
}

func TestResolveCachesRepeatedAncestorListingsWithinOperation(t *testing.T) {
	docs := newFakeDocuments(
		[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
		map[string][]anyshare.Item{
			"lib": {directory("one", "one")},
			"one": {directory("two", "two")},
		},
	)

	got, err := NewService(docs).Resolve(context.Background(), "//Library/one//two/.")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/Library/one/two" || got.ID != "two" || got.Type != "directory" {
		t.Fatalf("Resolve() = %#v", got)
	}
	if docs.entryCalls() != 1 || docs.listCount("lib") != 1 || docs.listCount("one") != 1 {
		t.Fatalf("calls entry=%d lib=%d one=%d, want 1 each", docs.entryCalls(), docs.listCount("lib"), docs.listCount("one"))
	}
}

func TestMkdirParentsCreatesOnlyMissingSegments(t *testing.T) {
	docs := newFakeDocuments(
		[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
		map[string][]anyshare.Item{
			"lib": {directory("existing", "existing")},
		},
	)

	got, err := NewService(docs).Mkdir(context.Background(), "/Library/existing/new/child", true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/Library/existing/new/child" || got.Type != "directory" {
		t.Fatalf("Mkdir() = %#v", got)
	}
	if got, want := docs.createdNames(), []string{"new", "child"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateDir names = %v, want %v", got, want)
	}
}

func TestMkdirUsesSuccessfulCreateResponseWithoutReconciliation(t *testing.T) {
	docs := newFakeDocuments([]anyshare.Item{library("lib", "Library", "user_doc_lib")}, map[string][]anyshare.Item{"lib": {}})
	docs.createItem = &anyshare.Item{ID: "created-id", DocID: "created-id", Name: "server-renamed", Type: "file", Rev: "created-r9"}
	docs.listErr = func(id string, calls int) error {
		if id == "lib" && calls > 1 {
			return errors.New("post-create reconciliation must not run")
		}
		return nil
	}

	got, err := NewService(docs).Mkdir(context.Background(), "/Library/requested", false)
	if err != nil {
		t.Fatal(err)
	}
	want := anyshare.Item{ID: "created-id", DocID: "created-id", Name: "requested", Path: "/Library/requested", Type: "directory", Rev: "created-r9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Mkdir() = %#v, want %#v", got, want)
	}
	if docs.createCount() != 1 || docs.listCount("lib") != 1 {
		t.Fatalf("creates=%d lists=%d, want exactly one create and initial list", docs.createCount(), docs.listCount("lib"))
	}
}

func TestMkdirReturnsDefinitiveCreateErrorWithoutReconciliation(t *testing.T) {
	docs := newFakeDocuments([]anyshare.Item{library("lib", "Library", "user_doc_lib")}, map[string][]anyshare.Item{"lib": {}})
	docs.createErr = apperr.Wrap(apperr.Interrupted, "create", "request interrupted", context.Canceled)

	_, err := NewService(docs).Mkdir(context.Background(), "/Library/requested", false)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Interrupted || !errors.Is(err, context.Canceled) {
		t.Fatalf("Mkdir() error = %v, want preserved interrupted cancellation", err)
	}
	if docs.createCount() != 1 || docs.listCount("lib") != 1 {
		t.Fatalf("creates=%d lists=%d, want exactly one create and initial list", docs.createCount(), docs.listCount("lib"))
	}
}

func TestMkdirExistingDirectorySucceedsAndExistingFileConflicts(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		docs := newFakeDocuments([]anyshare.Item{library("lib", "Library", "user_doc_lib")}, map[string][]anyshare.Item{
			"lib": {directory("dir", "present")},
		})
		got, err := NewService(docs).Mkdir(context.Background(), "/Library/present", false)
		if err != nil || got.ID != "dir" || docs.createCount() != 0 {
			t.Fatalf("Mkdir() = %#v, %v; creates=%d", got, err, docs.createCount())
		}
	})
	t.Run("file", func(t *testing.T) {
		docs := newFakeDocuments([]anyshare.Item{library("lib", "Library", "user_doc_lib")}, map[string][]anyshare.Item{
			"lib": {file("file", "present")},
		})
		_, err := NewService(docs).Mkdir(context.Background(), "/Library/present", false)
		assertRemoteError(t, err)
		if docs.createCount() != 0 {
			t.Fatalf("CreateDir calls = %d, want 0", docs.createCount())
		}
	})
}

func TestMkdirWithoutParentsDoesNotCreateMissingAncestor(t *testing.T) {
	docs := newFakeDocuments([]anyshare.Item{library("lib", "Library", "user_doc_lib")}, nil)
	_, err := NewService(docs).Mkdir(context.Background(), "/Library/missing/child", false)
	assertRemoteError(t, err)
	if docs.createCount() != 0 {
		t.Fatalf("CreateDir calls = %d, want 0", docs.createCount())
	}
}

func TestMkdirReconcilesLostSuccessAndRacesWithoutRetry(t *testing.T) {
	tests := []struct {
		name        string
		afterCreate func(*fakeDocuments, string, string)
		wantErr     bool
	}{
		{
			name: "lost success becomes directory",
			afterCreate: func(d *fakeDocuments, parentID, name string) {
				d.addChild(parentID, directory("created", name))
			},
		},
		{
			name: "directory race succeeds",
			afterCreate: func(d *fakeDocuments, parentID, name string) {
				d.addChild(parentID, directory("racer", name))
			},
		},
		{
			name: "file race conflicts",
			afterCreate: func(d *fakeDocuments, parentID, name string) {
				d.addChild(parentID, file("racer", name))
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs := newFakeDocuments([]anyshare.Item{library("lib", "Library", "user_doc_lib")}, map[string][]anyshare.Item{"lib": {}})
			docs.createErr = errors.New("response lost")
			docs.afterCreate = tt.afterCreate

			got, err := NewService(docs).Mkdir(context.Background(), "/Library/new", false)
			if tt.wantErr {
				assertRemoteError(t, err)
			} else if err != nil || got.Name != "new" || got.Path != "/Library/new" {
				t.Fatalf("Mkdir() = %#v, %v", got, err)
			}
			if docs.createCount() != 1 {
				t.Fatalf("CreateDir calls = %d, want 1", docs.createCount())
			}
			if docs.listCount("lib") != 2 {
				t.Fatalf("ListFolder(lib) calls = %d, want initial list plus reconciliation", docs.listCount("lib"))
			}
		})
	}
}

func TestResolveUploadTargetAppendsLocalBaseForDirectoriesAndSelectsLeaves(t *testing.T) {
	docs := newFakeDocuments(
		[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
		map[string][]anyshare.Item{
			"lib": {directory("dir", "dir"), file("file", "file.txt")},
			"dir": {},
		},
	)
	svc := NewService(docs)
	tests := []struct {
		name       string
		remotePath string
		wantPath   string
		wantParent string
		wantName   string
		wantExists bool
	}{
		{"trailing slash", "/Library/dir/", "/Library/dir/local.txt", "dir", "local.txt", false},
		{"existing directory", "/Library/dir", "/Library/dir/local.txt", "dir", "local.txt", false},
		{"existing file", "/Library/file.txt", "/Library/file.txt", "lib", "file.txt", true},
		{"new leaf", "/Library/new.txt", "/Library/new.txt", "lib", "new.txt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := svc.ResolveUploadTarget(context.Background(), "local.txt", tt.remotePath)
			if err != nil {
				t.Fatal(err)
			}
			if got.RemotePath != tt.wantPath || got.ParentID != tt.wantParent || got.Name != tt.wantName || (got.Existing != nil) != tt.wantExists {
				t.Fatalf("ResolveUploadTarget() = %#v", got)
			}
			if got.Existing != nil && got.Existing.Path != "/Library/file.txt" {
				t.Fatalf("Existing = %#v", got.Existing)
			}
		})
	}
}

func TestDeleteFileUsesResolvedExactIDWithoutRecursiveFlag(t *testing.T) {
	docs := newFakeDocuments(
		[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
		map[string][]anyshare.Item{"lib": {file("file-id", "report.txt")}},
	)
	docs.deleteResult = anyshare.DeleteResult{Status: anyshare.DeleteStatusPendingReview}

	result, err := NewService(docs).Delete(context.Background(), "//Library/./report.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	wantItem := file("file-id", "report.txt")
	wantItem.Path = "/Library/report.txt"
	if !reflect.DeepEqual(result.Item, wantItem) || result.Status != anyshare.DeleteStatusPendingReview {
		t.Fatalf("Delete() = %#v", result)
	}
	if got, want := docs.deleteCalls(), []deleteCall{{kind: "file", id: "file-id"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delete calls = %#v, want %#v", got, want)
	}
}

func TestDeleteDirectoryRequiresExplicitRecursive(t *testing.T) {
	t.Run("refuses directory without recursive", func(t *testing.T) {
		docs := newFakeDocuments(
			[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
			map[string][]anyshare.Item{"lib": {directory("dir-id", "folder")}},
		)
		_, err := NewService(docs).Delete(context.Background(), "/Library/folder", false)
		assertRemoteError(t, err)
		if got := docs.deleteCalls(); len(got) != 0 {
			t.Fatalf("delete calls = %#v, want none", got)
		}
	})

	t.Run("recursive deletes exact directory ID", func(t *testing.T) {
		docs := newFakeDocuments(
			[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
			map[string][]anyshare.Item{"lib": {directory("dir-id", "folder")}},
		)
		docs.deleteResult = anyshare.DeleteResult{Status: anyshare.DeleteStatusDeleted}
		result, err := NewService(docs).Delete(context.Background(), "/Library/folder", true)
		if err != nil {
			t.Fatal(err)
		}
		if result.Item.ID != "dir-id" || result.Item.Path != "/Library/folder" || result.Item.Type != "directory" || result.Status != anyshare.DeleteStatusDeleted {
			t.Fatalf("Delete() = %#v", result)
		}
		if got, want := docs.deleteCalls(), []deleteCall{{kind: "directory", id: "dir-id"}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("delete calls = %#v, want %#v", got, want)
		}
	})
}

func TestDeleteRefusesVirtualRootEntryLibraryAndUnknownType(t *testing.T) {
	tests := []struct {
		name string
		path string
		docs *fakeDocuments
	}{
		{
			name: "virtual root", path: "/",
			docs: newFakeDocuments([]anyshare.Item{library("lib", "Library", "user_doc_lib")}, nil),
		},
		{
			name: "entry library", path: "/Library/.",
			docs: newFakeDocuments([]anyshare.Item{library("lib", "Library", "user_doc_lib")}, nil),
		},
		{
			name: "unknown object type", path: "/Library/special",
			docs: newFakeDocuments(
				[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
				map[string][]anyshare.Item{"lib": {{ID: "special-id", Name: "special", Type: "special"}}},
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewService(test.docs).Delete(context.Background(), test.path, true)
			assertRemoteError(t, err)
			if got := test.docs.deleteCalls(); len(got) != 0 {
				t.Fatalf("delete calls = %#v, want none", got)
			}
		})
	}
}

func TestDeleteNeverReplaysOrClaimsAnyFailedDelete(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "ambiguous network failure",
			err:  apperr.Wrap(apperr.Network, "delete", "response lost", errors.New("transport error")),
		},
		{
			name: "request not sent",
			err:  apperr.Wrap(apperr.Network, "delete", "request not sent", anyshare.ErrRequestNotSent),
		},
		{
			name: "definitive remote rejection",
			err:  apperr.Wrap(apperr.Remote, "delete", "permission denied", errors.New("forbidden")),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			docs := newFakeDocuments(
				[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
				map[string][]anyshare.Item{"lib": {file("target-id", "report.txt")}},
			)
			docs.deleteErr = test.err
			docs.afterDelete = func(docs *fakeDocuments, _, _ string) { docs.setChildren("lib", nil) }
			_, err := NewService(docs).Delete(context.Background(), "/Library/report.txt", false)
			if !errors.Is(err, test.err) {
				t.Fatalf("Delete() error = %v, want original error", err)
			}
			if docs.entryCalls() != 1 || docs.listCount("lib") != 1 {
				t.Fatalf("resolution calls entry=%d list=%d, want no second resolution", docs.entryCalls(), docs.listCount("lib"))
			}
			if got, want := docs.deleteCalls(), []deleteCall{{kind: "file", id: "target-id"}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("delete calls = %#v, want exactly %#v", got, want)
			}
		})
	}
}

func library(id, name, typ string) anyshare.Item {
	return anyshare.Item{ID: id, DocID: id, Name: name, Type: typ, Rev: "lib-rev"}
}

func directory(id, name string) anyshare.Item {
	return anyshare.Item{ID: id, DocID: id, Name: name, Type: "directory", Rev: "dir-rev"}
}

func file(id, name string) anyshare.Item {
	return anyshare.Item{ID: id, DocID: id, Name: name, Type: "file", Rev: "file-rev"}
}

func itemNames(items []anyshare.Item) []string {
	names := make([]string, len(items))
	for i := range items {
		names[i] = items[i].Name
	}
	return names
}

func assertRemoteError(t *testing.T, err error) {
	t.Helper()
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
		t.Fatalf("error = %v, want remote category", err)
	}
}

type fakeDocuments struct {
	mu           sync.Mutex
	libraries    []anyshare.Item
	children     map[string][]anyshare.Item
	entry        int
	lists        map[string]int
	created      []string
	createErr    error
	createItem   *anyshare.Item
	listErr      func(id string, calls int) error
	afterCreate  func(*fakeDocuments, string, string)
	deleted      []deleteCall
	deleteResult anyshare.DeleteResult
	deleteErr    error
	afterDelete  func(*fakeDocuments, string, string)
}

type deleteCall struct {
	kind string
	id   string
}

func newFakeDocuments(libraries []anyshare.Item, children map[string][]anyshare.Item) *fakeDocuments {
	if children == nil {
		children = make(map[string][]anyshare.Item)
	}
	return &fakeDocuments{libraries: append([]anyshare.Item(nil), libraries...), children: children, lists: make(map[string]int)}
}

func (d *fakeDocuments) EntryDocLibs(context.Context) ([]anyshare.Item, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entry++
	return append([]anyshare.Item(nil), d.libraries...), nil
}

func (d *fakeDocuments) ListFolder(_ context.Context, id string) ([]anyshare.Item, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lists[id]++
	if d.listErr != nil {
		if err := d.listErr(id, d.lists[id]); err != nil {
			return nil, err
		}
	}
	return append([]anyshare.Item(nil), d.children[id]...), nil
}

func (d *fakeDocuments) CreateDir(_ context.Context, parentID, name string) (anyshare.Item, error) {
	d.mu.Lock()
	d.created = append(d.created, name)
	hook, err, response := d.afterCreate, d.createErr, d.createItem
	d.mu.Unlock()
	if hook != nil {
		hook(d, parentID, name)
	}
	if err != nil {
		return anyshare.Item{}, err
	}
	if response != nil {
		return *response, nil
	}
	item := directory(parentID+"/"+name, name)
	d.addChild(parentID, item)
	return item, nil
}

func (d *fakeDocuments) DeleteFile(_ context.Context, id string) (anyshare.DeleteResult, error) {
	return d.delete("file", id)
}

func (d *fakeDocuments) DeleteDir(_ context.Context, id string) (anyshare.DeleteResult, error) {
	return d.delete("directory", id)
}

func (d *fakeDocuments) delete(kind, id string) (anyshare.DeleteResult, error) {
	d.mu.Lock()
	d.deleted = append(d.deleted, deleteCall{kind: kind, id: id})
	hook, result, err := d.afterDelete, d.deleteResult, d.deleteErr
	d.mu.Unlock()
	if hook != nil {
		hook(d, kind, id)
	}
	if err != nil {
		return anyshare.DeleteResult{}, err
	}
	if result.Status == "" {
		result.Status = anyshare.DeleteStatusDeleted
	}
	return result, nil
}

func (d *fakeDocuments) addChild(parentID string, item anyshare.Item) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.children[parentID] = append(d.children[parentID], item)
}

func (d *fakeDocuments) setChildren(parentID string, items []anyshare.Item) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.children[parentID] = append([]anyshare.Item(nil), items...)
}

func (d *fakeDocuments) entryCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.entry
}

func (d *fakeDocuments) listCount(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lists[id]
}

func (d *fakeDocuments) createCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.created)
}

func (d *fakeDocuments) createdNames() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.created...)
}

func (d *fakeDocuments) deleteCalls() []deleteCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]deleteCall(nil), d.deleted...)
}
