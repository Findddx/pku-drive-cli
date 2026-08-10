package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/auth"
	"github.com/Findddx/pku-drive-cli/internal/cli"
	"github.com/Findddx/pku-drive-cli/internal/config"
	"github.com/Findddx/pku-drive-cli/internal/download"
	"github.com/Findddx/pku-drive-cli/internal/remote"
	"github.com/Findddx/pku-drive-cli/internal/upload"
	versionpkg "github.com/Findddx/pku-drive-cli/internal/version"
)

type retryTransport struct{ calls int }

func (transport *retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls++
	return &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("retry")),
		Request:    request,
	}, nil
}

type noOpBrowser struct{}

func (noOpBrowser) Open(context.Context, string) error { return nil }

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config", "pku-drive-cli")
	return config.Paths{
		ConfigDir:       configDir,
		ConfigFile:      filepath.Join(configDir, "config.json"),
		CredentialsFile: filepath.Join(configDir, "credentials.json"),
		CredentialLock:  filepath.Join(configDir, "credentials.lock"),
		UploadStateDir:  filepath.Join(root, "state", "pku-drive-cli", "uploads"),
	}
}

func TestNewWiredRuntimeAppliesRetryUploadAndStatePolicy(t *testing.T) {
	// Mutation caught: changing MaxAttempts=5, uploader concurrency=4, or selecting a state directory other than Paths.UploadStateDir.
	paths := testPaths(t)
	transport := new(retryTransport)
	runtime, err := newWiredRuntime(wireOptions{
		defaultPaths: func() (config.Paths, error) { return paths, nil },
		transport:    transport,
		browser:      noOpBrowser{},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.httpClient.Sleep = func(context.Context, time.Duration) error { return nil }
	request, err := http.NewRequest(http.MethodGet, "https://disk.example.invalid/retry", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtime.httpClient.Do(context.Background(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	if response != nil {
		_ = response.Body.Close()
	}
	if transport.calls != 5 {
		t.Fatalf("HTTP attempts=%d, want 5", transport.calls)
	}
	if runtime.uploader.Concurrency != 4 {
		t.Fatalf("upload concurrency=%d, want 4", runtime.uploader.Concurrency)
	}
	key := strings.Repeat("a", 64)
	if err := runtime.uploader.States.Save(key, upload.State{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(paths.UploadStateDir, key+".json")); err != nil {
		t.Fatalf("state path: %v", err)
	}
}

type authDouble struct {
	statuses []auth.Status
	calls    *[]string
	input    io.Reader
}

func (manager *authDouble) Login(_ context.Context, input io.Reader, _ io.Writer) (auth.Status, error) {
	manager.input = input
	return auth.Status{}, nil
}
func (manager *authDouble) Status(context.Context) (auth.Status, error) {
	*manager.calls = append(*manager.calls, "status")
	if len(manager.statuses) == 0 {
		return auth.Status{}, errors.New("unexpected status call")
	}
	status := manager.statuses[0]
	manager.statuses = manager.statuses[1:]
	return status, nil
}
func (manager *authDouble) Logout(context.Context, bool) error { return nil }

func TestDependenciesFromServicesEnablesCallbackInputOnlyExplicitly(t *testing.T) {
	manager := &authDouble{}
	deps := dependenciesFromServices(dependencyServices{manager: manager})
	input := strings.NewReader("fake callback")
	if err := deps.Login(context.Background(), input, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if manager.input != nil {
		t.Fatalf("default login forwarded callback input %T", manager.input)
	}
	if err := deps.Login(context.Background(), input, io.Discard, true); err != nil {
		t.Fatal(err)
	}
	if manager.input != input {
		t.Fatalf("paste login input=%T, want exact reader", manager.input)
	}
}

type identityDouble struct {
	user  anyshare.User
	calls *[]string
}

func (identity *identityDouble) CurrentUser(context.Context) (anyshare.User, error) {
	*identity.calls = append(*identity.calls, "current-user")
	return identity.user, nil
}

func TestDependenciesFromServicesStatusOrderAndRefreshedResult(t *testing.T) {
	// Mutation caught: skipping the initial local status, omitting CurrentUser, or returning pre-refresh server/expiry fields.
	calls := []string{}
	oldExpiry := time.Date(2026, 8, 9, 1, 0, 0, 0, time.UTC)
	newExpiry := time.Date(2026, 8, 9, 2, 0, 0, 0, time.UTC)
	deps := dependenciesFromServices(dependencyServices{
		manager: &authDouble{calls: &calls, statuses: []auth.Status{
			{LoggedIn: true, Server: "https://old.invalid", ExpiresAt: oldExpiry},
			{LoggedIn: true, Server: "https://refreshed.invalid", ExpiresAt: newExpiry},
		}},
		identity: &identityDouble{calls: &calls, user: anyshare.User{ID: "user-id", Name: "Alice", Account: "alice-account", Type: "user"}},
	})
	result, err := deps.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"status", "current-user", "status"}) {
		t.Fatalf("calls=%v", calls)
	}
	if !result.LoggedIn || result.Server != "https://refreshed.invalid" || result.Account != "alice-account" || result.ExpiresAt == nil || !result.ExpiresAt.Equal(newExpiry) {
		t.Fatalf("result=%+v", result)
	}
}

func TestDependenciesFromServicesStatusLoggedOutShortCircuits(t *testing.T) {
	// Mutation caught: contacting CurrentUser or returning success after the first local status reports logged out.
	calls := []string{}
	deps := dependenciesFromServices(dependencyServices{
		manager:  &authDouble{calls: &calls, statuses: []auth.Status{{LoggedIn: false}}},
		identity: &identityDouble{calls: &calls, user: anyshare.User{Account: "must-not-be-read"}},
	})
	_, err := deps.Status(context.Background())
	if apperr.ExitCode(err) != 3 || !reflect.DeepEqual(calls, []string{"status"}) {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
}

type remoteDouble struct {
	listItems         []anyshare.Item
	mkdirItem         anyshare.Item
	mkdirPath         string
	mkdirParents      bool
	resolveBase       string
	uploadResolvePath string
	resolvedTarget    remote.UploadTarget
	resolvedItem      anyshare.Item
	itemResolvePath   string
	deletePath        string
	deleteRecursive   bool
	deleteResult      remote.DeleteResult
}

func (service *remoteDouble) List(context.Context, string) ([]anyshare.Item, error) {
	return service.listItems, nil
}
func (service *remoteDouble) Mkdir(_ context.Context, path string, parents bool) (anyshare.Item, error) {
	service.mkdirPath, service.mkdirParents = path, parents
	return service.mkdirItem, nil
}
func (service *remoteDouble) ResolveUploadTarget(_ context.Context, base, path string) (remote.UploadTarget, error) {
	service.resolveBase, service.uploadResolvePath = base, path
	return service.resolvedTarget, nil
}
func (service *remoteDouble) Resolve(_ context.Context, path string) (anyshare.Item, error) {
	service.itemResolvePath = path
	return service.resolvedItem, nil
}
func (service *remoteDouble) Delete(_ context.Context, path string, recursive bool) (remote.DeleteResult, error) {
	service.deletePath, service.deleteRecursive = path, recursive
	return service.deleteResult, nil
}

type uploaderDouble struct {
	localPath string
	target    remote.UploadTarget
	overwrite bool
	progress  upload.Progress
	result    upload.Result
}

func (uploader *uploaderDouble) Put(_ context.Context, localPath string, target remote.UploadTarget, overwrite bool, progress upload.Progress) (upload.Result, error) {
	uploader.localPath, uploader.target, uploader.overwrite, uploader.progress = localPath, target, overwrite, progress
	return uploader.result, nil
}

type progressDouble struct{}

func (*progressDouble) Started(int64)  {}
func (*progressDouble) Advanced(int64) {}
func (*progressDouble) Finished()      {}

type downloaderDouble struct {
	item      anyshare.Item
	localPath string
	overwrite bool
	progress  download.Progress
	result    download.Result
}

func (downloader *downloaderDouble) Get(_ context.Context, item anyshare.Item, localPath string, overwrite bool, progress download.Progress) (download.Result, error) {
	downloader.item, downloader.localPath, downloader.overwrite, downloader.progress = item, localPath, overwrite, progress
	return downloader.result, nil
}

func TestDependenciesFromServicesMapsListAndMkdirItems(t *testing.T) {
	// Mutation caught: losing item fields, preferring DocID over ID, failing the DocID fallback, or dropping mkdir arguments.
	service := &remoteDouble{
		listItems: []anyshare.Item{
			{ID: "gns://preferred", DocID: "doc-not-used", Name: "one", Path: "/one", Type: "file", Size: 11, Modified: 12},
			{DocID: "doc-fallback", Name: "two", Path: "/two", Type: "directory", Size: 21, Modified: 22},
		},
		mkdirItem: anyshare.Item{DocID: "mkdir-doc", Name: "new", Path: "/parent/new", Type: "directory", Size: 31, Modified: 32},
	}
	deps := dependenciesFromServices(dependencyServices{remote: service})
	items, err := deps.List(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	wantItems := []cli.ItemResult{
		{Name: "one", RemotePath: "/one", Type: "file", RemoteID: "gns://preferred", Size: 11, Modified: 12},
		{Name: "two", RemotePath: "/two", Type: "directory", RemoteID: "doc-fallback", Size: 21, Modified: 22},
	}
	if !reflect.DeepEqual(items, wantItems) {
		t.Fatalf("items=%+v want=%+v", items, wantItems)
	}
	created, err := deps.Mkdir(context.Background(), "/parent/new", true)
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := cli.ItemResult{Name: "new", RemotePath: "/parent/new", Type: "directory", RemoteID: "mkdir-doc", Size: 31, Modified: 32}
	if created != wantCreated || service.mkdirPath != "/parent/new" || !service.mkdirParents {
		t.Fatalf("created=%+v path=%q parents=%v", created, service.mkdirPath, service.mkdirParents)
	}
}

func TestDependenciesFromServicesMapsPutInvocationAndResult(t *testing.T) {
	// Mutation caught: passing a full local path instead of its basename, changing target/overwrite/progress, or dropping any uploader Result field.
	target := remote.UploadTarget{RemotePath: "/docs/destination.bin", ParentID: "parent-id", Name: "destination.bin", Existing: &anyshare.Item{ID: "existing-id", Rev: "old-rev"}}
	service := &remoteDouble{resolvedTarget: target}
	uploader := &uploaderDouble{result: upload.Result{
		RemotePath: "/docs/destination.bin", RemoteID: "remote-id", Revision: "revision-2", Size: 42,
		Resumed: true, Instant: true, Multipart: true, PartsTotal: 9, PartsResumed: 4, PartsUploaded: 5,
	}}
	progress := new(progressDouble)
	deps := dependenciesFromServices(dependencyServices{remote: service, uploader: uploader})
	result, err := deps.Put(context.Background(), "/tmp/source.bin", "/docs/destination.bin", true, progress)
	if err != nil {
		t.Fatal(err)
	}
	if service.resolveBase != "source.bin" || service.uploadResolvePath != "/docs/destination.bin" {
		t.Fatalf("resolve base=%q path=%q", service.resolveBase, service.uploadResolvePath)
	}
	if uploader.localPath != "/tmp/source.bin" || !reflect.DeepEqual(uploader.target, target) || !uploader.overwrite || uploader.progress != progress {
		t.Fatalf("upload local=%q target=%+v overwrite=%v progress=%T", uploader.localPath, uploader.target, uploader.overwrite, uploader.progress)
	}
	want := cli.PutResult{RemotePath: "/docs/destination.bin", RemoteID: "remote-id", Revision: "revision-2", Size: 42, Resumed: true, Instant: true, Multipart: true, PartsTotal: 9, PartsResumed: 4, PartsUploaded: 5}
	if result != want {
		t.Fatalf("result=%+v want=%+v", result, want)
	}
}

func TestDependenciesFromServicesMapsGetAndDelete(t *testing.T) {
	remoteItem := anyshare.Item{ID: "gns://remote-file", Name: "remote.tsv", Path: "/Library/remote.tsv", Type: "file", Rev: "rev-3", Size: 17}
	service := &remoteDouble{
		resolvedItem: remoteItem,
		deleteResult: remote.DeleteResult{Item: anyshare.Item{DocID: "gns://deleted", Path: "/Library/folder", Type: "directory"}, Status: anyshare.DeleteStatusPendingReview},
	}
	destinationDir := t.TempDir()
	downloadService := &downloaderDouble{result: download.Result{LocalPath: filepath.Join(destinationDir, "remote.tsv"), RemoteID: "gns://remote-file", Revision: "rev-3", Size: 17}}
	progress := new(progressDouble)
	deps := dependenciesFromServices(dependencyServices{remote: service, downloader: downloadService})

	got, err := deps.Get(context.Background(), "/Library/remote.tsv", destinationDir, true, progress)
	if err != nil {
		t.Fatal(err)
	}
	if service.itemResolvePath != "/Library/remote.tsv" || downloadService.item != remoteItem || downloadService.localPath != filepath.Join(destinationDir, "remote.tsv") || !downloadService.overwrite || downloadService.progress != progress {
		t.Fatalf("service=%+v downloader=%+v", service, downloadService)
	}
	wantGet := cli.GetResult{RemotePath: "/Library/remote.tsv", RemoteID: "gns://remote-file", Revision: "rev-3", LocalPath: filepath.Join(destinationDir, "remote.tsv"), Size: 17}
	if got != wantGet {
		t.Fatalf("get=%+v want=%+v", got, wantGet)
	}

	deleted, err := deps.Delete(context.Background(), "/Library/folder", true)
	if err != nil {
		t.Fatal(err)
	}
	wantDelete := cli.DeleteResult{RemotePath: "/Library/folder", RemoteID: "gns://deleted", Type: "directory", Status: "pending_review", PendingReview: true}
	if deleted != wantDelete || service.deletePath != "/Library/folder" || !service.deleteRecursive {
		t.Fatalf("delete=%+v service=%+v", deleted, service)
	}
}

func TestDownloadDestinationDirectoryAndTrailingSlashRules(t *testing.T) {
	directory := t.TempDir()
	got, err := downloadDestination(directory, "报告.tsv")
	if err != nil || got != filepath.Join(directory, "报告.tsv") {
		t.Fatalf("destination=%q err=%v", got, err)
	}
	if _, err := downloadDestination(filepath.Join(directory, "missing")+string(filepath.Separator), "report.tsv"); apperr.ExitCode(err) != 6 {
		t.Fatalf("missing directory err=%v", err)
	}
	if _, err := downloadDestination(directory, "../escape"); apperr.ExitCode(err) != 6 {
		t.Fatalf("unsafe remote name err=%v", err)
	}
}

func TestWireDependenciesFailureIsLocalAndDoesNotBlockVersion(t *testing.T) {
	// Mutation caught: eagerly failing the process when path/config construction fails or exposing the raw local cause.
	deps := wireDependenciesWith(wireOptions{defaultPaths: func() (config.Paths, error) { return config.Paths{}, errors.New("fake-sensitive-local-cause") }})
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"version"}, strings.NewReader(""), &stdout, &stderr, deps, versionpkg.Info{Version: "1.2.3", Commit: "abc", BuildDate: "date"})
	if code != 0 || stdout.String() != "pku-drive 1.2.3 (abc) date\n" || stderr.Len() != 0 {
		t.Fatalf("version code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	code = cli.Run(context.Background(), []string{"status", "--json"}, strings.NewReader(""), &stdout, &stderr, deps, versionpkg.Info{})
	if code != 6 || stdout.String() != "{\"category\":\"local\",\"message\":\"local dependency unavailable\",\"ok\":false}\n" || strings.Contains(stdout.String(), "fake-sensitive-local-cause") {
		t.Fatalf("status code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
