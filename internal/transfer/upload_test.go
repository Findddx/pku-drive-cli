package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/remote"
	"github.com/Findddx/pku-drive-cli/internal/upload"
)

type mkdirCall struct {
	path    string
	parents bool
}

type fakeRemoteDirs struct {
	mu       sync.Mutex
	mkdirs   []mkdirCall
	lists    []string
	tree     map[string][]anyshare.Item // remote path -> children (by name)
	mkdirIDs map[string]string          // remote path -> assigned ID
	seq      int
}

func (f *fakeRemoteDirs) Mkdir(_ context.Context, path string, parents bool) (anyshare.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mkdirs = append(f.mkdirs, mkdirCall{path: path, parents: parents})
	if _, exists := f.mkdirIDs[path]; !exists {
		f.seq++
		f.mkdirIDs[path] = "dir-id-" + path
	}
	return anyshare.Item{ID: f.mkdirIDs[path], Name: "dir", Path: path, Type: "directory", Size: -1}, nil
}

func (f *fakeRemoteDirs) List(_ context.Context, path string) ([]anyshare.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, path)
	children := f.tree[path]
	return append([]anyshare.Item(nil), children...), nil
}

func (f *fakeRemoteDirs) mkdirPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := make([]string, len(f.mkdirs))
	for i, call := range f.mkdirs {
		paths[i] = call.path
	}
	return paths
}

func (f *fakeRemoteDirs) listPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := make([]string, len(f.lists))
	copy(paths, f.lists)
	return paths
}

func (f *fakeRemoteDirs) setChildren(path string, children []anyshare.Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tree[path] = children
}

type fakeBatchUploader struct {
	mu         sync.Mutex
	puts       []remote.UploadTarget
	locals     []string
	overwrites []bool
	errPath    string
	err        error
}

func (f *fakeBatchUploader) Put(_ context.Context, localPath string, target remote.UploadTarget,
	overwrite bool, progress upload.Progress) (upload.Result, error) {
	f.mu.Lock()
	f.puts = append(f.puts, target)
	f.locals = append(f.locals, localPath)
	f.overwrites = append(f.overwrites, overwrite)
	f.mu.Unlock()
	if f.errPath != "" && target.RemotePath == f.errPath {
		if f.err != nil {
			return upload.Result{}, f.err
		}
		return upload.Result{}, apperr.Wrap(apperr.Remote, "put", "upload failed", errors.New("conflict"))
	}
	return upload.Result{RemotePath: target.RemotePath, RemoteID: "doc-" + target.Name, Revision: "r1", Size: 1}, nil
}

func (f *fakeBatchUploader) putNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(f.puts))
	for i, target := range f.puts {
		names[i] = target.Name
	}
	return names
}

func localTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "数据"))
	mkdir(t, filepath.Join(root, "数据", "deep"))
	write(t, filepath.Join(root, "a.bin"), 0o600)
	write(t, filepath.Join(root, "数据", "b.bin"), 0o600)
	write(t, filepath.Join(root, "数据", "deep", "c.bin"), 0o600)
	return root
}

func TestUploadTreeTwoPhaseDirectoryBeforeFile(t *testing.T) {
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{}, mkdirIDs: map[string]string{}}
	uploader := new(fakeBatchUploader)
	root := localTree(t)

	report, err := UploadTree(context.Background(), dirs, uploader, root, "/dst", Options{Jobs: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Completed) != 3 || len(report.Skipped) != 0 {
		t.Fatalf("completed=%d skipped=%v", len(report.Completed), report.Skipped)
	}
	// Directory creation happens before any file upload, ascending depth.
	wantMkdirs := []string{"/dst", "/dst/数据", "/dst/数据/deep"}
	if !reflect.DeepEqual(dirs.mkdirPaths(), wantMkdirs) {
		t.Fatalf("mkdirs=%v want=%v", dirs.mkdirPaths(), wantMkdirs)
	}
	for _, call := range dirs.mkdirs {
		if !call.parents {
			t.Fatalf("mkdir without parents: %+v", call)
		}
	}
	// Each directory listed exactly once.
	if !reflect.DeepEqual(dirs.listPaths(), wantMkdirs) {
		t.Fatalf("lists=%v want=%v", dirs.listPaths(), wantMkdirs)
	}
	// Files sorted by relative path.
	wantRel := []string{"a.bin", "数据/b.bin", "数据/deep/c.bin"}
	for i, want := range wantRel {
		if report.Completed[i].Relative != want {
			t.Fatalf("completed[%d]=%q want=%q", i, report.Completed[i].Relative, want)
		}
	}
	// Targets carry the plan's parent IDs.
	for _, target := range uploader.puts {
		if target.ParentID != "dir-id-"+filepath.ToSlash(filepath.Dir(target.RemotePath)[1:]) && target.ParentID == "" {
			t.Fatalf("target without parent: %+v", target)
		}
	}
}

func TestUploadTreeSkipExistingWithoutOverwriteNoPut(t *testing.T) {
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{
		"/dst": {{ID: "doc-a", Name: "a.bin", Path: "/dst/a.bin", Type: "file", Rev: "old", Size: 1}},
	}, mkdirIDs: map[string]string{}}
	uploader := new(fakeBatchUploader)
	root := localTree(t)

	report, err := UploadTree(context.Background(), dirs, uploader, root, "/dst", Options{Jobs: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Completed) != 2 || len(report.Skipped) != 1 || report.Skipped[0].Path != "a.bin" || report.Skipped[0].Reason != SkipExists {
		t.Fatalf("completed=%d skipped=%v", len(report.Completed), report.Skipped)
	}
	if names := uploader.putNames(); len(names) != 2 {
		t.Fatalf("puts=%v (a.bin must not be uploaded)", names)
	}
}

func TestUploadTreeOverwriteForwardsExisting(t *testing.T) {
	existing := anyshare.Item{ID: "doc-a", Name: "a.bin", Path: "/dst/a.bin", Type: "file", Rev: "old", Size: 1}
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{"/dst": {existing}}, mkdirIDs: map[string]string{}}
	uploader := new(fakeBatchUploader)
	root := localTree(t)

	_, err := UploadTree(context.Background(), dirs, uploader, root, "/dst", Options{Jobs: 1, Overwrite: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found *remote.UploadTarget
	for i, target := range uploader.puts {
		if target.Name == "a.bin" {
			found = &uploader.puts[i]
		}
	}
	if found == nil {
		t.Fatal("a.bin not uploaded with --overwrite")
	}
	if found.Existing == nil || found.Existing.ID != "doc-a" {
		t.Fatalf("existing=%+v", found.Existing)
	}
	if !uploader.overwrites[0] {
		t.Fatal("overwrite not forwarded")
	}
}

func TestUploadTreeRemoteDirectoryConflictStops(t *testing.T) {
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{
		"/dst": {{ID: "dir-x", Name: "a.bin", Path: "/dst/a.bin", Type: "directory", Rev: "d", Size: -1}},
	}, mkdirIDs: map[string]string{}}
	uploader := new(fakeBatchUploader)
	root := localTree(t)

	_, err := UploadTree(context.Background(), dirs, uploader, root, "/dst", Options{Jobs: 1}, nil)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
		t.Fatalf("err=%v want Remote conflict", err)
	}
	if len(uploader.puts) != 0 {
		t.Fatalf("puts=%v", uploader.putNames())
	}
}

func TestUploadTreeEmptyLocalDirectoriesCreateNothing(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "空目录"))
	mkdir(t, filepath.Join(root, "空目录", "嵌套"))
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{}, mkdirIDs: map[string]string{}}
	uploader := new(fakeBatchUploader)

	report, err := UploadTree(context.Background(), dirs, uploader, root, "/dst", Options{Jobs: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Completed) != 0 || len(report.Skipped) != 0 {
		t.Fatalf("report=%+v", report)
	}
	if got := dirs.mkdirPaths(); len(got) != 0 {
		t.Fatalf("mkdirs=%v want zero Mkdir calls", got)
	}
}

func TestUploadTreeSymlinkSkipsDoNotReachPut(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "real.bin"), 0o600)
	if err := os.Symlink(filepath.Join(root, "real.bin"), filepath.Join(root, "link.bin")); err != nil {
		t.Fatal(err)
	}
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{}, mkdirIDs: map[string]string{}}
	uploader := new(fakeBatchUploader)

	report, err := UploadTree(context.Background(), dirs, uploader, root, "/dst", Options{Jobs: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Completed) != 1 || len(report.Skipped) != 1 || report.Skipped[0].Reason != SkipSymlink {
		t.Fatalf("report=%+v", report)
	}
	if names := uploader.putNames(); !reflect.DeepEqual(names, []string{"real.bin"}) {
		t.Fatalf("puts=%v", names)
	}
}

func TestUploadTreeRemoteFileUnderLocalDirectoryConflicts(t *testing.T) {
	// Remote /备份/a is a file; local ./src/a/ is a directory.
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{
		"/备份": {{ID: "file-a", Name: "a", Path: "/备份/a", Type: "file", Rev: "f", Size: 1}},
	}, mkdirIDs: map[string]string{}}
	// The Mkdir for /备份/a must surface as a conflict; simulate by having the
	// fake return the remoteConflict via tree state is not enough—Mkdir here
	// succeeds. Instead the conflict surfaces during List of /备份: the child
	// named "a" is a file. Per plan §5.1: "Mkdir 返回 remoteConflict → Remote
	// 错停止". Model the fake Mkdir to detect the file conflict itself.
	conflictDirs := &conflictMkdirDirs{fakeRemoteDirs: dirs}

	root := t.TempDir()
	mkdir(t, filepath.Join(root, "a"))
	write(t, filepath.Join(root, "a", "inner.bin"), 0o600)

	_, err := UploadTree(context.Background(), conflictDirs, new(fakeBatchUploader), root, "/备份", Options{Jobs: 1}, nil)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
		t.Fatalf("err=%v want Remote", err)
	}
}

type conflictMkdirDirs struct {
	*fakeRemoteDirs
}

func (f *conflictMkdirDirs) Mkdir(ctx context.Context, path string, parents bool) (anyshare.Item, error) {
	f.mu.Lock()
	children := f.tree["/备份"]
	f.mu.Unlock()
	for _, child := range children {
		if child.Name == baseName(path) && child.Type == "file" {
			return anyshare.Item{}, apperr.Wrap(apperr.Remote, "mkdir", "remote conflict: "+path, errors.New("file exists at directory path"))
		}
	}
	return f.fakeRemoteDirs.Mkdir(ctx, path, parents)
}

func TestUploadTreeFirstFileFailureStopsAndKeepsCompleted(t *testing.T) {
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{}, mkdirIDs: map[string]string{}}
	uploader := &fakeBatchUploader{errPath: "/dst/数据/b.bin", err: apperr.Wrap(apperr.Network, "put", "upload failed", errors.New("reset"))}
	root := localTree(t)

	report, err := UploadTree(context.Background(), dirs, uploader, root, "/dst", Options{Jobs: 1}, nil)
	if err == nil {
		t.Fatal("want error")
	}
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Network {
		t.Fatalf("err=%v want Network", err)
	}
	if len(report.Completed) != 1 || report.Completed[0].Relative != "a.bin" {
		t.Fatalf("completed=%+v", report.Completed)
	}
}

func TestUploadTreeVirtualRootIsUsage(t *testing.T) {
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{}, mkdirIDs: map[string]string{}}
	_, err := UploadTree(context.Background(), dirs, new(fakeBatchUploader), t.TempDir(), "/", Options{Jobs: 1}, nil)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Usage {
		t.Fatalf("err=%v want Usage", err)
	}
	if got := dirs.mkdirPaths(); len(got) != 0 {
		t.Fatalf("mkdirs=%v (validation must precede any request)", got)
	}
}

func TestUploadTreeMissingLocalRootIsLocalError(t *testing.T) {
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{}, mkdirIDs: map[string]string{}}
	_, err := UploadTree(context.Background(), dirs, new(fakeBatchUploader), filepath.Join(t.TempDir(), "missing"), "/dst", Options{Jobs: 1}, nil)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Local {
		t.Fatalf("err=%v want Local", err)
	}
}

func TestUploadTreeSingleFileRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "only.bin"), 0o600)
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{}, mkdirIDs: map[string]string{}}
	uploader := new(fakeBatchUploader)

	report, err := UploadTree(context.Background(), dirs, uploader, root, "/dst", Options{Jobs: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Completed) != 1 || report.Completed[0].Relative != "only.bin" {
		t.Fatalf("report=%+v", report)
	}
	if got := dirs.mkdirPaths(); !reflect.DeepEqual(got, []string{"/dst"}) {
		t.Fatalf("mkdirs=%v", got)
	}
}

func TestUploadTreeJobsFourCompletes(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		write(t, filepath.Join(root, "f"+string(rune('0'+i))+".bin"), 0o600)
	}
	dirs := &fakeRemoteDirs{tree: map[string][]anyshare.Item{}, mkdirIDs: map[string]string{}}
	uploader := new(fakeBatchUploader)

	report, err := UploadTree(context.Background(), dirs, uploader, root, "/dst", Options{Jobs: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Completed) != 5 {
		t.Fatalf("completed=%d", len(report.Completed))
	}
}
