package download_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/download"
)

func TestGetVerifiesMetadataAndAtomicallyWritesFile(t *testing.T) {
	const contents = "verified download contents"
	item := fileItem(contents)
	api := &downloadAPIFake{
		metadata:       item,
		authorizations: []anyshare.SignedRequest{{Method: "GET", URL: "https://objects.invalid/one"}},
		streams:        []streamResult{{stream: anyshare.DownloadStream{Body: io.NopCloser(strings.NewReader(contents)), ContentLength: int64(len(contents))}}},
	}
	progress := &progressRecorder{}
	destination := filepath.Join(t.TempDir(), "renamed-local.tsv")

	result, err := download.NewDownloader(api).Get(context.Background(), item, destination, false, progress)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != contents {
		t.Fatalf("contents = %q", data)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	abs, _ := filepath.Abs(destination)
	if result.LocalPath != abs || result.RemoteID != item.ID || result.Revision != item.Rev || result.Size != int64(len(contents)) {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(api.metadataCalls, [][2]string{{item.ID, item.Rev}}) {
		t.Fatalf("metadata calls = %#v", api.metadataCalls)
	}
	if !reflect.DeepEqual(api.authorizationCalls, [][3]string{{item.ID, item.Rev, item.Name}}) {
		t.Fatalf("authorization calls = %#v", api.authorizationCalls)
	}
	if !reflect.DeepEqual(progress.started, []int64{int64(len(contents))}) || progress.advanced != int64(len(contents)) || progress.finished != 1 {
		t.Fatalf("progress = %#v", progress)
	}
	assertDirectoryEntries(t, filepath.Dir(destination), filepath.Base(destination))
}

func TestGetRefusesExistingDestinationWithoutNetwork(t *testing.T) {
	const original = "keep original"
	destination := filepath.Join(t.TempDir(), "existing.bin")
	if err := os.WriteFile(destination, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	api := &downloadAPIFake{}
	_, err := download.NewDownloader(api).Get(context.Background(), fileItem("new"), destination, false, nil)
	assertCategory(t, err, apperr.Local)
	data, _ := os.ReadFile(destination)
	if string(data) != original || len(api.metadataCalls) != 0 {
		t.Fatalf("destination=%q metadataCalls=%v", data, api.metadataCalls)
	}
}

func TestGetOverwriteReplacesOnlyAfterSuccessfulVerification(t *testing.T) {
	const contents = "replacement contents"
	item := fileItem(contents)
	destination := filepath.Join(t.TempDir(), "existing.bin")
	if err := os.WriteFile(destination, []byte("old contents"), 0o640); err != nil {
		t.Fatal(err)
	}
	api := successfulAPI(item, contents)
	if _, err := download.NewDownloader(api).Get(context.Background(), item, destination, true, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(destination)
	if string(data) != contents {
		t.Fatalf("destination = %q", data)
	}
	assertDirectoryEntries(t, filepath.Dir(destination), filepath.Base(destination))
}

func TestGetRejectsMetadataDriftBeforeAuthorization(t *testing.T) {
	const contents = "contents"
	base := fileItem(contents)
	tests := []struct {
		name   string
		mutate func(*anyshare.Item)
	}{
		{name: "identity", mutate: func(item *anyshare.Item) { item.ID = "gns://other" }},
		{name: "revision", mutate: func(item *anyshare.Item) { item.Rev = "revision-8" }},
		{name: "size", mutate: func(item *anyshare.Item) { item.Size++ }},
		{name: "type", mutate: func(item *anyshare.Item) { item.Type = "directory" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := base
			test.mutate(&metadata)
			api := &downloadAPIFake{metadata: metadata}
			destination := filepath.Join(t.TempDir(), "target")
			_, err := download.NewDownloader(api).Get(context.Background(), base, destination, false, nil)
			assertCategory(t, err, apperr.Integrity)
			if len(api.authorizationCalls) != 0 {
				t.Fatalf("authorization calls = %v", api.authorizationCalls)
			}
			assertDirectoryEntries(t, filepath.Dir(destination))
		})
	}
}

func TestGetRejectsLengthOrChecksumMismatchAndPreservesOverwriteTarget(t *testing.T) {
	const contents = "expected contents"
	base := fileItem(contents)
	tests := []struct {
		name          string
		body          string
		contentLength int64
		metadataMD5   string
	}{
		{name: "declared length", body: contents, contentLength: int64(len(contents) - 1), metadataMD5: base.MD5},
		{name: "short body", body: contents[:len(contents)-1], contentLength: -1, metadataMD5: base.MD5},
		{name: "long body", body: contents + "x", contentLength: -1, metadataMD5: base.MD5},
		{name: "md5", body: contents, contentLength: int64(len(contents)), metadataMD5: strings.Repeat("0", 32)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := base
			metadata.MD5 = test.metadataMD5
			api := &downloadAPIFake{
				metadata:       metadata,
				authorizations: []anyshare.SignedRequest{{Method: "GET", URL: "https://objects.invalid/item"}},
				streams: []streamResult{{stream: anyshare.DownloadStream{
					Body: io.NopCloser(strings.NewReader(test.body)), ContentLength: test.contentLength,
				}}},
			}
			destination := filepath.Join(t.TempDir(), "existing")
			if err := os.WriteFile(destination, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := download.NewDownloader(api).Get(context.Background(), base, destination, true, nil)
			assertCategory(t, err, apperr.Integrity)
			data, _ := os.ReadFile(destination)
			if string(data) != "original" {
				t.Fatalf("destination changed to %q", data)
			}
			assertDirectoryEntries(t, filepath.Dir(destination), filepath.Base(destination))
		})
	}
}

func TestGetRefreshesExpiredSignatureAtMostOnce(t *testing.T) {
	const contents = "refreshed contents"
	item := fileItem(contents)
	api := &downloadAPIFake{
		metadata: item,
		authorizations: []anyshare.SignedRequest{
			{Method: "GET", URL: "https://objects.invalid/expired"},
			{Method: "GET", URL: "https://objects.invalid/refreshed"},
		},
		streams: []streamResult{
			{err: anyshare.ErrExpiredSignature},
			{stream: anyshare.DownloadStream{Body: io.NopCloser(strings.NewReader(contents)), ContentLength: int64(len(contents))}},
		},
	}
	destination := filepath.Join(t.TempDir(), "target")
	if _, err := download.NewDownloader(api).Get(context.Background(), item, destination, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(api.authorizationCalls) != 2 || api.getCalls != 2 {
		t.Fatalf("authorization=%d get=%d", len(api.authorizationCalls), api.getCalls)
	}

	api = &downloadAPIFake{
		metadata:       item,
		authorizations: []anyshare.SignedRequest{{Method: "GET"}, {Method: "GET"}, {Method: "GET"}},
		streams:        []streamResult{{err: anyshare.ErrExpiredSignature}, {err: anyshare.ErrExpiredSignature}},
	}
	destination = filepath.Join(t.TempDir(), "target")
	if _, err := download.NewDownloader(api).Get(context.Background(), item, destination, false, nil); err == nil {
		t.Fatal("expected second signature expiration to fail")
	}
	if len(api.authorizationCalls) != 2 || api.getCalls != 2 {
		t.Fatalf("authorization=%d get=%d, want exactly two", len(api.authorizationCalls), api.getCalls)
	}
	assertDirectoryEntries(t, filepath.Dir(destination))
}

func TestGetCleansTemporaryFileOnStreamFailure(t *testing.T) {
	item := fileItem("expected")
	api := &downloadAPIFake{
		metadata:       item,
		authorizations: []anyshare.SignedRequest{{Method: "GET"}},
		streams: []streamResult{{stream: anyshare.DownloadStream{
			Body:          &failingReadCloser{first: []byte("part"), err: errors.New("synthetic network failure")},
			ContentLength: -1,
		}}},
	}
	destination := filepath.Join(t.TempDir(), "target")
	_, err := download.NewDownloader(api).Get(context.Background(), item, destination, false, nil)
	assertCategory(t, err, apperr.Network)
	assertDirectoryEntries(t, filepath.Dir(destination))
}

func TestGetNoOverwriteWinsDestinationRace(t *testing.T) {
	const contents = "download"
	item := fileItem(contents)
	directory := t.TempDir()
	destination := filepath.Join(directory, "target")
	body := &callbackReadCloser{Reader: strings.NewReader(contents), callback: func() {
		if err := os.WriteFile(destination, []byte("racing writer"), 0o600); err != nil {
			t.Error(err)
		}
	}}
	api := &downloadAPIFake{
		metadata:       item,
		authorizations: []anyshare.SignedRequest{{Method: "GET"}},
		streams:        []streamResult{{stream: anyshare.DownloadStream{Body: body, ContentLength: int64(len(contents))}}},
	}
	_, err := download.NewDownloader(api).Get(context.Background(), item, destination, false, nil)
	assertCategory(t, err, apperr.Local)
	data, _ := os.ReadFile(destination)
	if string(data) != "racing writer" {
		t.Fatalf("racing destination changed to %q", data)
	}
	assertDirectoryEntries(t, directory, "target")
}

type streamResult struct {
	stream anyshare.DownloadStream
	err    error
}

type downloadAPIFake struct {
	metadata           anyshare.Item
	metadataErr        error
	metadataCalls      [][2]string
	authorizations     []anyshare.SignedRequest
	authorizationErr   error
	authorizationCalls [][3]string
	streams            []streamResult
	getCalls           int
}

func (f *downloadAPIFake) FileMetadata(_ context.Context, docID, rev string) (anyshare.Item, error) {
	f.metadataCalls = append(f.metadataCalls, [2]string{docID, rev})
	return f.metadata, f.metadataErr
}

func (f *downloadAPIFake) AuthorizeDownload(_ context.Context, docID, rev, saveName string) (anyshare.SignedRequest, error) {
	f.authorizationCalls = append(f.authorizationCalls, [3]string{docID, rev, saveName})
	if f.authorizationErr != nil {
		return anyshare.SignedRequest{}, f.authorizationErr
	}
	index := len(f.authorizationCalls) - 1
	if index >= len(f.authorizations) {
		return anyshare.SignedRequest{}, errors.New("unexpected authorization call")
	}
	return f.authorizations[index], nil
}

func (f *downloadAPIFake) GetSigned(_ context.Context, _ anyshare.SignedRequest) (anyshare.DownloadStream, error) {
	index := f.getCalls
	f.getCalls++
	if index >= len(f.streams) {
		return anyshare.DownloadStream{}, errors.New("unexpected object call")
	}
	return f.streams[index].stream, f.streams[index].err
}

type progressRecorder struct {
	started  []int64
	advanced int64
	finished int
}

func (p *progressRecorder) Started(total int64)  { p.started = append(p.started, total) }
func (p *progressRecorder) Advanced(delta int64) { p.advanced += delta }
func (p *progressRecorder) Finished()            { p.finished++ }

type failingReadCloser struct {
	first []byte
	err   error
}

func (r *failingReadCloser) Read(p []byte) (int, error) {
	if len(r.first) != 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	return 0, r.err
}
func (*failingReadCloser) Close() error { return nil }

type callbackReadCloser struct {
	io.Reader
	callback func()
	called   bool
}

func (r *callbackReadCloser) Read(p []byte) (int, error) {
	if !r.called {
		r.called = true
		r.callback()
	}
	return r.Reader.Read(p)
}
func (*callbackReadCloser) Close() error { return nil }

func fileItem(contents string) anyshare.Item {
	sum := md5.Sum([]byte(contents))
	return anyshare.Item{
		ID: "gns://library/file", Name: "remote.tsv", Type: "file", Rev: "revision-7",
		Size: int64(len(contents)), MD5: strings.ToUpper(hex.EncodeToString(sum[:])),
	}
}

func successfulAPI(item anyshare.Item, contents string) *downloadAPIFake {
	return &downloadAPIFake{
		metadata:       item,
		authorizations: []anyshare.SignedRequest{{Method: "GET", URL: "https://objects.invalid/item"}},
		streams: []streamResult{{stream: anyshare.DownloadStream{
			Body: io.NopCloser(strings.NewReader(contents)), ContentLength: int64(len(contents)),
		}}},
	}
}

func assertCategory(t *testing.T, err error, want apperr.Category) {
	t.Helper()
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != want {
		t.Fatalf("err = %v, want category %s", err, want)
	}
}

func assertDirectoryEntries(t *testing.T, directory string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Fatalf("directory entries = %v, want %v", entryNames(entries), want)
	}
	for index := range want {
		if entries[index].Name() != want[index] {
			t.Fatalf("directory entries = %v, want %v", entryNames(entries), want)
		}
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}
