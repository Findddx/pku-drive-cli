package remote

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

func walkTree() (*fakeDocuments, *Service) {
	docs := newFakeDocuments(
		[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
		map[string][]anyshare.Item{
			"lib": {directory("d1", "alpha"), file("f1", "a.txt"), directory("d2", "beta"), file("f2", "z.txt")},
			"d1":  {file("f3", "deep.csv"), directory("d3", "nested")},
			"d3":  {file("f4", "leaf.bin")},
			"d2":  {},
		},
	)
	return docs, NewService(docs)
}

func TestWalkFilesFileRootReturnsSingleEntry(t *testing.T) {
	docs, svc := walkTree()
	got, err := svc.WalkFiles(context.Background(), "/Library/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := []anyshare.Item{file("f1", "a.txt")}
	want[0].Path = "/Library/a.txt"
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
	if docs.listCount("lib") != 1 {
		t.Fatalf("ListFolder(lib)=%d want 1", docs.listCount("lib"))
	}
}

func TestWalkFilesDepthFirstLexicalOrder(t *testing.T) {
	// Files come before directories at the same level because the walk is
	// depth-first over the sorted listing and files are leaves: the concrete
	// order is the sorted-name order with each directory's subtree emitted
	// when the directory is reached.
	docs, svc := walkTree()
	got, err := svc.WalkFiles(context.Background(), "/Library")
	if err != nil {
		t.Fatal(err)
	}
	gotPaths := make([]string, len(got))
	for i, item := range got {
		gotPaths[i] = item.Path
		if item.Type != "file" || item.Rev == "" || itemID(item) == "" {
			t.Fatalf("entry %#v lacks download identity", item)
		}
	}
	// Sorted same-level names: "a.txt" < "alpha" < "beta" < "z.txt" ('.'
	// sorts before letters); each directory's subtree is emitted at its
	// directory position.
	wantPaths := []string{
		"/Library/a.txt",
		"/Library/alpha/deep.csv",
		"/Library/alpha/nested/leaf.bin",
		"/Library/z.txt",
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("paths=%v want=%v", gotPaths, wantPaths)
	}
	if docs.entryCalls() != 1 {
		t.Fatalf("EntryDocLibs=%d want 1", docs.entryCalls())
	}
	for id, want := range map[string]int{"lib": 1, "d1": 1, "d3": 1, "d2": 1} {
		if got := docs.listCount(id); got != want {
			t.Fatalf("ListFolder(%s)=%d want %d", id, got, want)
		}
	}
}

func TestWalkFilesLibraryRootAllowed(t *testing.T) {
	_, svc := walkTree()
	got, err := svc.WalkFiles(context.Background(), "/Library")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("entries=%#v", got)
	}
	// A library root with no children yields an empty (non-nil) result and
	// lists the library exactly once.
	other := newFakeDocuments(
		[]anyshare.Item{library("lib2", "空库", "shared_doc_lib")},
		map[string][]anyshare.Item{"lib2": {}},
	)
	empty, err := NewService(other).WalkFiles(context.Background(), "/空库")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty library entries=%#v", empty)
	}
	if other.listCount("lib2") != 1 {
		t.Fatalf("ListFolder(lib2)=%d want 1", other.listCount("lib2"))
	}
}

func TestWalkFilesVirtualRootIsUsage(t *testing.T) {
	docs, svc := walkTree()
	_, err := svc.WalkFiles(context.Background(), "/")
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Usage {
		t.Fatalf("err=%v want Usage", err)
	}
	if docs.entryCalls() != 0 {
		t.Fatalf("EntryDocLibs=%d want 0 (guard precedes requests)", docs.entryCalls())
	}
}

func TestWalkFilesNonAbsoluteIsUsage(t *testing.T) {
	_, svc := walkTree()
	_, err := svc.WalkFiles(context.Background(), "relative/path")
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Usage {
		t.Fatalf("err=%v want Usage", err)
	}
}

func TestWalkFilesNotFoundIsRemote(t *testing.T) {
	_, svc := walkTree()
	_, err := svc.WalkFiles(context.Background(), "/Library/missing")
	assertRemoteError(t, err)
}

func TestWalkFilesMissingFileIdentityIsRemote(t *testing.T) {
	for name, mutate := range map[string]func(item *anyshare.Item){
		"missing rev":   func(item *anyshare.Item) { item.Rev = "" },
		"missing id":    func(item *anyshare.Item) { item.ID, item.DocID = "", "" },
		"negative size": func(item *anyshare.Item) { item.Size = -5 },
	} {
		t.Run(name, func(t *testing.T) {
			docs := newFakeDocuments(
				[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
				map[string][]anyshare.Item{"lib": {file("f1", "a.txt")}},
			)
			children := docs.children["lib"]
			mutate(&children[0])
			docs.setChildren("lib", children)
			_, err := NewService(docs).WalkFiles(context.Background(), "/Library")
			assertRemoteError(t, err)
		})
	}
}

func TestWalkFilesUnknownTypeIsRemote(t *testing.T) {
	docs := newFakeDocuments(
		[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
		map[string][]anyshare.Item{"lib": {{ID: "x", Name: "special", Type: "special", Rev: "r"}}},
	)
	_, err := NewService(docs).WalkFiles(context.Background(), "/Library")
	assertRemoteError(t, err)
}

func TestWalkFilesDuplicateNamesInOneListingConflict(t *testing.T) {
	docs := newFakeDocuments(
		[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
		map[string][]anyshare.Item{
			"lib": {file("f1", "dup"), directory("d1", "dup")},
			"d1":  {file("f2", "inner.txt")},
		},
	)
	_, err := NewService(docs).WalkFiles(context.Background(), "/Library")
	assertRemoteError(t, err)
}

func TestWalkFilesRootTypeAnomalyIsRemote(t *testing.T) {
	docs := newFakeDocuments(
		[]anyshare.Item{library("lib", "Library", "user_doc_lib")},
		map[string][]anyshare.Item{"lib": {{ID: "s", Name: "special", Type: "special"}}},
	)
	_, err := NewService(docs).WalkFiles(context.Background(), "/Library/special")
	assertRemoteError(t, err)
}

func TestWalkFilesEachDirectoryListedExactlyOnce(t *testing.T) {
	docs, svc := walkTree()
	if _, err := svc.WalkFiles(context.Background(), "//Library/./"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"lib": 1, "d1": 1, "d2": 1, "d3": 1} {
		if got := docs.listCount(id); got != want {
			t.Fatalf("ListFolder(%s)=%d want %d (resolver cache must dedupe)", id, got, want)
		}
	}
}
