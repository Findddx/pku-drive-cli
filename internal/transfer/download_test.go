package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/download"
)

type fakeRemoteTree struct {
	mu    sync.Mutex
	roots map[string][]anyshare.Item
	calls []string
}

func (f *fakeRemoteTree) WalkFiles(_ context.Context, root string) ([]anyshare.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, root)
	items, ok := f.roots[root]
	if !ok {
		return nil, apperr.Wrap(apperr.Remote, "walk files", "not found: "+root, errors.New("missing"))
	}
	return append([]anyshare.Item(nil), items...), nil
}

type fakeBatchDownloader struct {
	mu         sync.Mutex
	gets       []string // relative paths
	items      []anyshare.Item
	overwrites []bool
	results    map[string]download.Result
	errAt      string
	err        error
	progress   download.Progress
}

func (f *fakeBatchDownloader) GetAt(_ context.Context, item anyshare.Item, root *download.DestinationRoot,
	relativePath string, overwrite bool, progress download.Progress) (download.Result, error) {
	f.mu.Lock()
	if f.results == nil {
		f.results = map[string]download.Result{}
	}
	result, hasResult := f.results[relativePath]
	f.mu.Unlock()
	_ = root
	f.mu.Lock()
	f.gets = append(f.gets, relativePath)
	f.items = append(f.items, item)
	f.overwrites = append(f.overwrites, overwrite)
	f.progress = progress
	f.mu.Unlock()
	if relativePath == f.errAt {
		return download.Result{}, f.err
	}
	if hasResult {
		return result, nil
	}
	return download.Result{LocalPath: relativePath, RemoteID: item.ID, Revision: item.Rev, Size: item.Size}, nil
}

type recordingProgress struct {
	mu       sync.Mutex
	started  []int64
	advanced []int64
	finished int
}

func (p *recordingProgress) Started(total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = append(p.started, total)
}

func (p *recordingProgress) Advanced(delta int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.advanced = append(p.advanced, delta)
}

func (p *recordingProgress) Finished() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finished++
}

func treeItems() []anyshare.Item {
	return []anyshare.Item{
		{ID: "doc-1", Name: "a.csv", Path: "/src/a.csv", Type: "file", Rev: "r1", Size: 10},
		{ID: "doc-2", Name: "b.csv", Path: "/src/数据/b.csv", Type: "file", Rev: "r2", Size: 20},
		{ID: "doc-3", Name: "c.csv", Path: "/src/数据/deep/c.csv", Type: "file", Rev: "r3", Size: 30},
	}
}

func TestDownloadTreeFullSuccessSortedReport(t *testing.T) {
	tree := &fakeRemoteTree{roots: map[string][]anyshare.Item{"/src": treeItems()}}
	downloader := new(fakeBatchDownloader)
	progress := new(recordingProgress)
	dst := t.TempDir()

	report, err := DownloadTree(context.Background(), tree, downloader, "/src", dst, Options{Jobs: 1}, progress)
	if err != nil {
		t.Fatal(err)
	}
	wantRel := []string{"a.csv", "数据/b.csv", "数据/deep/c.csv"}
	gotRel := make([]string, len(report.Completed))
	for i, c := range report.Completed {
		gotRel[i] = c.Relative
		if c.RemotePath != "/src/"+c.Relative {
			t.Fatalf("remotePath=%q relative=%q", c.RemotePath, c.Relative)
		}
	}
	if !reflect.DeepEqual(gotRel, wantRel) {
		t.Fatalf("completed=%v want=%v", gotRel, wantRel)
	}
	if len(report.Skipped) != 0 {
		t.Fatalf("skipped=%v", report.Skipped)
	}
	if len(progress.started) != 1 || progress.started[0] != 60 {
		t.Fatalf("started=%v want=[60]", progress.started)
	}
	if progress.finished != 1 {
		t.Fatalf("finished=%d", progress.finished)
	}
	// Files land on disk through the real DestinationRoot? No—the fake
	// downloader writes nothing; only preflight creates parent dirs.
	if _, statErr := os.Stat(filepath.Join(dst, "a.csv")); statErr == nil {
		t.Fatal("fake downloader must not create files")
	}
}

func TestDownloadTreeSkipExistingWithoutOverwrite(t *testing.T) {
	tree := &fakeRemoteTree{roots: map[string][]anyshare.Item{"/src": treeItems()}}
	downloader := new(fakeBatchDownloader)
	dst := t.TempDir()
	existing := filepath.Join(dst, "a.csv")
	if err := os.WriteFile(existing, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := DownloadTree(context.Background(), tree, downloader, "/src", dst, Options{Jobs: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Completed) != 2 || len(report.Skipped) != 1 || report.Skipped[0].Path != "a.csv" || report.Skipped[0].Reason != SkipExists {
		t.Fatalf("completed=%d skipped=%v", len(report.Completed), report.Skipped)
	}
	for _, get := range downloader.gets {
		if get == "a.csv" {
			t.Fatal("skipped file was still downloaded")
		}
	}
	// Existing file untouched.
	data, _ := os.ReadFile(existing)
	if string(data) != "old" {
		t.Fatalf("existing file changed: %q", data)
	}
}

func TestDownloadTreeOverwriteForwardsToDownloader(t *testing.T) {
	tree := &fakeRemoteTree{roots: map[string][]anyshare.Item{"/src": treeItems()[:1]}}
	downloader := new(fakeBatchDownloader)
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "a.csv"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := DownloadTree(context.Background(), tree, downloader, "/src", dst, Options{Jobs: 1, Overwrite: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Completed) != 1 || len(report.Skipped) != 0 {
		t.Fatalf("completed=%d skipped=%v", len(report.Completed), report.Skipped)
	}
	if !downloader.overwrites[0] {
		t.Fatal("overwrite not forwarded")
	}
}

func TestDownloadTreeStopsAtFirstFileError(t *testing.T) {
	tree := &fakeRemoteTree{roots: map[string][]anyshare.Item{"/src": treeItems()}}
	downloader := &fakeBatchDownloader{errAt: "数据/b.csv", err: apperr.Wrap(apperr.Network, "get", "download failed", errors.New("reset"))}
	dst := t.TempDir()

	report, err := DownloadTree(context.Background(), tree, downloader, "/src", dst, Options{Jobs: 1}, nil)
	if err == nil {
		t.Fatal("want error")
	}
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Network {
		t.Fatalf("err=%v want Network", err)
	}
	if len(report.Completed) != 1 || report.Completed[0].Relative != "a.csv" {
		t.Fatalf("completed=%+v", report.Completed)
	}
	if downloader.errAt == "a.csv" {
		t.Fatal("sanity")
	}
}

func TestDownloadTreeMissingLocalDirIsLocalError(t *testing.T) {
	tree := &fakeRemoteTree{roots: map[string][]anyshare.Item{"/src": treeItems()}}
	downloader := new(fakeBatchDownloader)
	_, err := DownloadTree(context.Background(), tree, downloader, "/src", filepath.Join(t.TempDir(), "missing"), Options{Jobs: 1}, nil)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Local {
		t.Fatalf("err=%v want Local", err)
	}
}

func TestDownloadTreeVirtualRootIsUsage(t *testing.T) {
	tree := &fakeRemoteTree{roots: map[string][]anyshare.Item{}}
	downloader := new(fakeBatchDownloader)
	_, err := DownloadTree(context.Background(), tree, downloader, "/", t.TempDir(), Options{Jobs: 1}, nil)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Usage {
		t.Fatalf("err=%v want Usage", err)
	}
	if len(tree.calls) != 0 {
		t.Fatalf("WalkFiles calls=%v want zero (validation before network)", tree.calls)
	}
}

func TestDownloadTreeRelativeRemotePathIsUsage(t *testing.T) {
	tree := &fakeRemoteTree{roots: map[string][]anyshare.Item{}}
	_, err := DownloadTree(context.Background(), tree, new(fakeBatchDownloader), "relative", t.TempDir(), Options{Jobs: 1}, nil)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Usage {
		t.Fatalf("err=%v want Usage", err)
	}
}

func TestDownloadTreeJobsFourConcurrentRealRoots(t *testing.T) {
	items := make([]anyshare.Item, 8)
	for i := range items {
		items[i] = anyshare.Item{ID: "doc", Name: "f", Path: "/src/f" + string(rune('0'+i)) + ".csv", Type: "file", Rev: "r", Size: 1}
	}
	tree := &fakeRemoteTree{roots: map[string][]anyshare.Item{"/src": items}}
	downloader := new(fakeBatchDownloader)
	progress := new(recordingProgress)
	dst := t.TempDir()

	// Wrap the fake to observe peak concurrency against the real
	// DestinationRoot shared by all workers.
	peakConcurrency := &peakTracker{}
	downloader2 := &concurrencyDownloader{inner: downloader, tracker: peakConcurrency}

	report, err := DownloadTree(context.Background(), tree, downloader2, "/src", dst, Options{Jobs: 4}, progress)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Completed) != 8 {
		t.Fatalf("completed=%d", len(report.Completed))
	}
	if peakConcurrency.peak() > 4 {
		t.Fatalf("peak concurrency=%d exceeds jobs=4", peakConcurrency.peak())
	}
	if peakConcurrency.peak() < 2 {
		t.Fatalf("peak concurrency=%d never overlapped", peakConcurrency.peak())
	}
	if len(progress.started) != 1 || progress.started[0] != 8 {
		t.Fatalf("started=%v want total=8", progress.started)
	}
}

type peakTracker struct {
	mu     sync.Mutex
	active int
	max    int
}

func (p *peakTracker) enter() {
	p.mu.Lock()
	p.active++
	if p.active > p.max {
		p.max = p.active
	}
	p.mu.Unlock()
}

func (p *peakTracker) leave() {
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
}

func (p *peakTracker) peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

type concurrencyDownloader struct {
	inner   *fakeBatchDownloader
	tracker *peakTracker
}

func (c *concurrencyDownloader) GetAt(ctx context.Context, item anyshare.Item, root *download.DestinationRoot,
	relativePath string, overwrite bool, progress download.Progress) (download.Result, error) {
	c.tracker.enter()
	defer c.tracker.leave()
	// A tiny sleep widens the overlap window so peak concurrency is
	// observable while keeping the test fast.
	time.Sleep(5 * time.Millisecond)
	return c.inner.GetAt(ctx, item, root, relativePath, overwrite, progress)
}
