package upload_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/upload"
)

func TestStateKeyUsesUnambiguousLowercaseSHA256(t *testing.T) {
	got := upload.StateKey("https://disk.pku.edu.cn", "/个人文档/a.bin", "/data/a.bin")
	if got != "9a1e47fbc15f49d413f7f63be269bb8d1526575bc845a478479f6618005214e7" {
		t.Fatalf("key=%q", got)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) {
		t.Fatalf("key is not lowercase SHA-256 hex: %q", got)
	}
	first := upload.StateKey("ab", "c", "/d")
	second := upload.StateKey("a", "bc", "/d")
	if first == second {
		t.Fatalf("ambiguous tuples collided at %q", first)
	}
}

func TestStateStoreSaveLoadAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	store := upload.NewStateStore(dir)
	key := upload.StateKey("server", "/remote", "/local")
	want := fixtureState()

	if err := store.Save(key, want); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode=%o, want 700", dirInfo.Mode().Perm())
	}
	path := filepath.Join(dir, key+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode=%o, want 600", info.Mode().Perm())
	}
	got, err := store.Load(key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state=%+v, want %+v", got, want)
	}
}

func TestStateStoreRoundTripsOverwriteEditedRevision(t *testing.T) {
	// Production mutation caught: omitting the begin-time overwrite baseline from durable resume JSON.
	dir := filepath.Join(t.TempDir(), "uploads")
	store := upload.NewStateStore(dir)
	key := upload.StateKey("server", "/remote", "/local")
	want := fixtureState()
	want.EditedRev = "original-revision"
	if err := store.Save(key, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(key)
	if err != nil {
		t.Fatal(err)
	}
	if got.EditedRev != "original-revision" || !reflect.DeepEqual(got, want) {
		t.Fatalf("state=%+v, want %+v", got, want)
	}
}

func TestStateStoreRoundTripsSecretFreeOperationPhase(t *testing.T) {
	// Production mutation caught: dropping the durable non-idempotent operation phase or serializing request credentials with it.
	dir := filepath.Join(t.TempDir(), "uploads")
	store := upload.NewStateStore(dir)
	key := upload.StateKey("server", "/remote", "/local")
	want := fixtureState()
	want.Phase = upload.PhaseFinishAttempted
	if err := store.Save(key, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != upload.PhaseFinishAttempted || !reflect.DeepEqual(got, want) {
		t.Fatalf("state=%+v want=%+v", got, want)
	}
	encoded, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"signed_request", "authorization", "oauth", "completion_xml"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("state contains forbidden field %q: %s", forbidden, encoded)
		}
	}
}

func TestStateStorePreservesNilAndEmptyCompleted(t *testing.T) {
	for _, completed := range []map[int]upload.CompletedPart{nil, {}} {
		dir := filepath.Join(t.TempDir(), "uploads")
		store := upload.NewStateStore(dir)
		key := upload.StateKey("server", "/remote", "/local")
		want := fixtureState()
		want.Completed = completed
		if err := store.Save(key, want); err != nil {
			t.Fatal(err)
		}
		got, err := store.Load(key)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Completed, completed) {
			t.Fatalf("completed=%#v, want %#v", got.Completed, completed)
		}
	}
}

func TestStateStoreRejectsInvalidKeysWithoutEscapingDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "uploads")
	store := upload.NewStateStore(dir)
	state := fixtureState()
	invalid := []string{"", "abc", "../outside", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "gggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg"}
	for _, key := range invalid {
		if err := store.Save(key, state); err == nil {
			t.Fatalf("Save(%q) succeeded", key)
		} else {
			assertCategory(t, err, apperr.Usage)
		}
		if _, err := store.Load(key); err == nil {
			t.Fatalf("Load(%q) succeeded", key)
		} else {
			assertCategory(t, err, apperr.Usage)
		}
		if err := store.Remove(key); err == nil {
			t.Fatalf("Remove(%q) succeeded", key)
		} else {
			assertCategory(t, err, apperr.Usage)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "outside.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside path was touched: %v", err)
	}
}

func TestStateStoreRejectsUnsafeDirectoryAndEntries(t *testing.T) {
	t.Run("relative directory", func(t *testing.T) {
		store := upload.NewStateStore("relative/uploads")
		err := store.Save(upload.StateKey("s", "/r", "/l"), fixtureState())
		assertCategory(t, err, apperr.Local)
	})
	t.Run("symlink directory", func(t *testing.T) {
		root := t.TempDir()
		realDir := filepath.Join(root, "real")
		if err := os.Mkdir(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "uploads")
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		store := upload.NewStateStore(link)
		err := store.Save(upload.StateKey("s", "/r", "/l"), fixtureState())
		assertCategory(t, err, apperr.Local)
	})
	t.Run("symlink state", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "uploads")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		key := upload.StateKey("s", "/r", "/l")
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, key+".json")); err != nil {
			t.Fatal(err)
		}
		store := upload.NewStateStore(dir)
		err := store.Save(key, fixtureState())
		assertCategory(t, err, apperr.Local)
		contents, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != "sentinel" {
			t.Fatalf("symlink target changed to %q", contents)
		}
	})
}

func TestStateStoreRejectsCorruptAndTrailingJSONWithoutDeleting(t *testing.T) {
	for _, contents := range []string{"{", "{} {}"} {
		dir := filepath.Join(t.TempDir(), "uploads")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		key := upload.StateKey("s", "/r", "/l")
		path := filepath.Join(dir, key+".json")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		store := upload.NewStateStore(dir)
		_, err := store.Load(key)
		assertCategory(t, err, apperr.Integrity)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != contents {
			t.Fatalf("corrupt state changed to %q", got)
		}
	}
}

func TestStateStoreRemoveIsExactAndIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	store := upload.NewStateStore(dir)
	first := upload.StateKey("server", "/one", "/local")
	second := upload.StateKey("server", "/two", "/local")
	state := fixtureState()
	state.LocalPath = filepath.Join(t.TempDir(), "must-not-be-used")
	if err := os.WriteFile(state.LocalPath, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(first, state); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(second, state); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(first); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, first+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected state still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, second+".json")); err != nil {
		t.Fatalf("other state missing: %v", err)
	}
	contents, err := os.ReadFile(state.LocalPath)
	if err != nil || string(contents) != "sentinel" {
		t.Fatalf("state path was used: contents=%q err=%v", contents, err)
	}
}

func TestStateMatchesFullFingerprint(t *testing.T) {
	state := fixtureState()
	if !state.Matches(state.Fingerprint) {
		t.Fatal("identical fingerprint did not match")
	}
	mutations := []upload.Fingerprint{
		{Device: 2, Inode: 2, Size: 42, MtimeNS: 3, MD5: "A", SliceMD5: "B", CRC32: "C"},
		{Device: 1, Inode: 3, Size: 42, MtimeNS: 3, MD5: "A", SliceMD5: "B", CRC32: "C"},
		{Device: 1, Inode: 2, Size: 43, MtimeNS: 3, MD5: "A", SliceMD5: "B", CRC32: "C"},
		{Device: 1, Inode: 2, Size: 42, MtimeNS: 4, MD5: "A", SliceMD5: "B", CRC32: "C"},
		{Device: 1, Inode: 2, Size: 42, MtimeNS: 3, MD5: "X", SliceMD5: "B", CRC32: "C"},
		{Device: 1, Inode: 2, Size: 42, MtimeNS: 3, MD5: "A", SliceMD5: "X", CRC32: "C"},
		{Device: 1, Inode: 2, Size: 42, MtimeNS: 3, MD5: "A", SliceMD5: "B", CRC32: "X"},
	}
	for _, mutation := range mutations {
		if state.Matches(mutation) {
			t.Fatalf("mismatched fingerprint accepted: %+v", mutation)
		}
	}
}

func TestStateStoreUploadLockSerializesAndAllowsStateSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	store := upload.NewStateStore(dir)
	key := upload.StateKey("server", "/remote", "/local")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	var active atomic.Int32
	var maxActive atomic.Int32

	go func() {
		firstDone <- store.WithUploadLock(context.Background(), key, func() error {
			updateMax(&active, &maxActive, 1)
			defer active.Add(-1)
			close(firstEntered)
			<-releaseFirst
			return store.Save(key, fixtureState())
		})
	}()
	<-firstEntered

	secondCheckedLock := make(chan struct{})
	secondCtx := &observedContext{Context: context.Background(), secondCheck: secondCheckedLock}
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- store.WithUploadLock(secondCtx, key, func() error {
			updateMax(&active, &maxActive, 1)
			defer active.Add(-1)
			close(secondEntered)
			return nil
		})
	}()
	<-secondCheckedLock

	select {
	case <-secondEntered:
		t.Fatal("second callback entered while first held the lock")
	default:
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if maxActive.Load() != 1 {
		t.Fatalf("maximum concurrent callbacks=%d, want 1", maxActive.Load())
	}
	info, err := os.Stat(filepath.Join(dir, key+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode=%o, want 600", info.Mode().Perm())
	}
}

func TestStateStoreUploadLockHonorsCanceledWaiter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	store := upload.NewStateStore(dir)
	key := upload.StateKey("server", "/remote", "/local")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.WithUploadLock(context.Background(), key, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	baseCtx, cancel := context.WithCancel(context.Background())
	secondCheckedLock := make(chan struct{})
	ctx := &observedContext{Context: baseCtx, secondCheck: secondCheckedLock}
	called := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- store.WithUploadLock(ctx, key, func() error {
			close(called)
			return nil
		})
	}()
	<-secondCheckedLock
	cancel()
	err := <-secondDone
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context canceled", err)
	}
	select {
	case <-called:
		t.Fatal("canceled lock waiter invoked callback")
	default:
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestStateStoreUploadLockReturnsCallbackError(t *testing.T) {
	store := upload.NewStateStore(filepath.Join(t.TempDir(), "uploads"))
	key := upload.StateKey("server", "/remote", "/local")
	want := errors.New("upload failed")
	err := store.WithUploadLock(context.Background(), key, func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want callback error", err)
	}
}

func fixtureState() upload.State {
	return upload.State{
		Server: "https://disk.pku.edu.cn", RemotePath: "/个人文档/a.bin", LocalPath: "/data/a.bin",
		Fingerprint: upload.Fingerprint{Device: 1, Inode: 2, Size: 42, MtimeNS: 3, MD5: "A", SliceMD5: "B", CRC32: "C"},
		DocID:       "gns://doc", Rev: "rev", UploadID: "upload", PartSize: 4,
		Completed: map[int]upload.CompletedPart{1: {ETag: "etag", Size: 4}},
		CreatedAt: time.Date(2026, 8, 9, 1, 2, 3, 4, time.UTC),
		UpdatedAt: time.Date(2026, 8, 9, 5, 6, 7, 8, time.UTC),
	}
}

func updateMax(active, maximum *atomic.Int32, delta int32) {
	current := active.Add(delta)
	for {
		previous := maximum.Load()
		if current <= previous || maximum.CompareAndSwap(previous, current) {
			return
		}
	}
}

type observedContext struct {
	context.Context
	checks      atomic.Int32
	readyOnce   sync.Once
	secondCheck chan struct{}
}

func (c *observedContext) Err() error {
	err := c.Context.Err()
	if c.checks.Add(1) == 2 {
		c.readyOnce.Do(func() { close(c.secondCheck) })
	}
	return err
}
