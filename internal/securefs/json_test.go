package securefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPathOperationsRejectNonCanonicalFullPath(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		dir := privateTestDir(t)
		canonical := filepath.Join(dir, "credentials.json")
		if err := WriteJSONAtomic(dir+"//credentials.json", map[string]string{"token": "value"}); err == nil {
			t.Fatal("expected non-canonical write path rejection")
		}
		if _, err := os.Lstat(canonical); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("non-canonical write reached canonical entry: %v", err)
		}
	})
	t.Run("read", func(t *testing.T) {
		dir := privateTestDir(t)
		canonical := filepath.Join(dir, "credentials.json")
		if err := os.WriteFile(canonical, []byte(`{"token":"value"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := ReadJSON0600(dir+"//credentials.json", &got); err == nil {
			t.Fatal("expected non-canonical read path rejection")
		}
	})
	t.Run("remove", func(t *testing.T) {
		dir := privateTestDir(t)
		canonical := filepath.Join(dir, "credentials.json")
		if err := os.WriteFile(canonical, []byte(`{"token":"value"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Remove0600(dir + "//credentials.json"); err == nil {
			t.Fatal("expected non-canonical remove path rejection")
		}
		if _, err := os.Stat(canonical); err != nil {
			t.Fatalf("non-canonical remove deleted canonical entry: %v", err)
		}
	})
	t.Run("lock", func(t *testing.T) {
		dir := privateTestDir(t)
		called := false
		if err := WithLock(context.Background(), dir+"//credentials.lock", func() error {
			called = true
			return nil
		}); err == nil {
			t.Fatal("expected non-canonical lock path rejection")
		}
		if called {
			t.Fatal("non-canonical lock path invoked callback")
		}
		if _, err := os.Lstat(filepath.Join(dir, "credentials.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("non-canonical lock reached canonical entry: %v", err)
		}
	})
}

type failingJSON struct{}

func (failingJSON) MarshalJSON() ([]byte, error) {
	return nil, errors.New("encode failed")
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pku-drive-cli")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWriteJSONAtomicReplacesFileWith0600JSON(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "credentials.json")
	if err := WriteJSONAtomic(path, map[string]string{"token": "fresh"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("mode = %o, want %o", got, want)
	}
	var got map[string]string
	if err := ReadJSON0600(path, &got); err != nil {
		t.Fatal(err)
	}
	if got["token"] != "fresh" {
		t.Fatalf("token = %q, want fresh", got["token"])
	}
}

func TestWriteJSONAtomicKeepsOriginalWhenEncodingFails(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "credentials.json")
	want := []byte("{\"token\":\"original\"}\n")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONAtomic(path, failingJSON{}); err == nil {
		t.Fatal("expected encoding error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("original = %q, want %q", got, want)
	}
}

func TestReadJSON0600RejectsWidePermissions(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"token":"value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := ReadJSON0600(path, &got); err == nil {
		t.Fatal("expected permission error")
	}
}

func TestPrivateFileValidationRejectsWrongUID(t *testing.T) {
	stat := syscall.Stat_t{
		Mode: syscall.S_IFREG | 0o600,
		Uid:  uint32(os.Geteuid() + 1),
	}
	if err := validateRegular0600("object-pin.json", &stat); err == nil {
		t.Fatal("expected wrong-UID private file rejection")
	}
}

func TestSensitiveFileOperationsRejectNonRegularEntries(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		path := filepath.Join(privateTestDir(t), "credentials.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := ReadJSON0600(path, &got); err == nil {
			t.Fatal("expected non-regular read rejection")
		}
	})
	t.Run("write destination", func(t *testing.T) {
		path := filepath.Join(privateTestDir(t), "credentials.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := WriteJSONAtomic(path, map[string]string{"token": "value"}); err == nil {
			t.Fatal("expected non-regular destination rejection")
		}
	})
	t.Run("remove", func(t *testing.T) {
		path := filepath.Join(privateTestDir(t), "credentials.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := Remove0600(path); err == nil {
			t.Fatal("expected non-regular remove rejection")
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("unsafe entry changed: info=%v err=%v", info, err)
		}
	})
}

func TestSensitiveFileValidationRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "credentials.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var got map[string]string
		done <- ReadJSON0600(path, &got)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected FIFO rejection")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("FIFO validation blocked instead of rejecting the non-regular entry")
	}
}

func TestWriteJSONAtomicCreatesExact0600DespiteUmask(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "credentials.json")
	old := syscall.Umask(0o777)
	defer syscall.Umask(old)
	if err := WriteJSONAtomic(path, map[string]string{"token": "value"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky); got != 0o600 {
		t.Fatalf("mode=%v, want exact 0600", got)
	}
}

func TestReadJSON0600RejectsSpecialPermissionBits(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"token":"value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o4600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Skip("filesystem does not preserve setuid bits for this user")
	}
	var got map[string]string
	if err := ReadJSON0600(path, &got); err == nil {
		t.Fatal("expected special permission bits to be rejected")
	}
}

func TestReadJSON0600RejectsTruncatedJSON(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"token":`), 0o600); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := ReadJSON0600(path, &got); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestReadJSON0600RejectsTrailingJSONValueAndGarbage(t *testing.T) {
	for _, contents := range []string{
		`{"token":"value"} {"another":"value"}`,
		`{"token":"value"} garbage`,
	} {
		path := filepath.Join(privateTestDir(t), "credentials.json")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := ReadJSON0600(path, &got); err == nil {
			t.Fatalf("expected trailing input rejection for %q", contents)
		}
	}
}

func TestReadJSON0600RejectsSymlink(t *testing.T) {
	dir := privateTestDir(t)
	target := filepath.Join(dir, "target.json")
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(target, []byte(`{"token":"value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := ReadJSON0600(path, &got); err == nil {
		t.Fatal("expected symlink error")
	}
}

func TestWriteJSONAtomicRejectsSymlinkDestination(t *testing.T) {
	dir := privateTestDir(t)
	target := filepath.Join(dir, "target.json")
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(target, []byte(`{"token":"original"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONAtomic(path, map[string]string{"token": "replacement"}); err == nil {
		t.Fatal("expected symlink error")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"token":"original"}` {
		t.Fatalf("target = %q, want original", got)
	}
}

func TestDirectoryBoundWriteSurvivesLeafSwap(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "pku-drive-cli")
	dir, err := EnsurePrivateDir(leaf)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	original := filepath.Join(root, "original")
	if err := os.Rename(leaf, original); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(root, "redirect")
	if err := os.Mkdir(redirect, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(redirect, leaf); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONAtomicAt(dir, "credentials.json", map[string]string{"token": "bound"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(original, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{\"token\":\"bound\"}\n" {
		t.Fatalf("original leaf = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(redirect, "credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("redirect received a write: %v", err)
	}
}

func TestDirectoryBoundDeleteSurvivesLeafSwap(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "pku-drive-cli")
	dir, err := EnsurePrivateDir(leaf)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := WriteJSONAtomicAt(dir, "credentials.json", map[string]string{"token": "bound"}); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, "original")
	if err := os.Rename(leaf, original); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(root, "redirect")
	if err := os.Mkdir(redirect, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(redirect, leaf); err != nil {
		t.Fatal(err)
	}
	if err := Remove0600At(dir, "credentials.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(original, "credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original was not deleted: %v", err)
	}
}

func TestRemove0600AtDoesNotDeleteUnvalidatedLeafReplacement(t *testing.T) {
	dirPath := privateTestDir(t)
	dir, err := OpenPrivateDir(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	const name = "credentials.json"
	path := filepath.Join(dirPath, name)
	moved := filepath.Join(dirPath, "validated-original.json")
	if err := os.WriteFile(path, []byte(`{"token":"original"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ops := hostDeletionOps()
	renameNoReplace := ops.renameNoReplace
	unlinkat := ops.unlinkat
	swapped := false
	installReplacement := func() error {
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(`{"token":"unsafe replacement"}`), 0o644); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o644); err != nil {
			return err
		}
		swapped = true
		return nil
	}
	ops.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		if oldName == name && !swapped {
			if err := installReplacement(); err != nil {
				return err
			}
		}
		return renameNoReplace(oldDirFD, oldName, newDirFD, newName)
	}
	ops.unlinkat = func(dirfd int, candidate string) error {
		if candidate == name && !swapped {
			if err := installReplacement(); err != nil {
				return err
			}
		}
		return unlinkat(dirfd, candidate)
	}
	if err := remove0600AtWithOps(dir, name, ops); err == nil {
		t.Fatal("expected unsafe isolated candidate rejection")
	}
	if !swapped {
		t.Fatal("pre-isolation replacement was not exercised")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("unvalidated replacement was deleted: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("replacement mode=%o, want unchanged 0644", got)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("original file was deleted: %v", err)
	}
}

func TestRemove0600AtDeletesNormalCandidateAndIsIdempotent(t *testing.T) {
	dirPath := privateTestDir(t)
	dir, err := OpenPrivateDir(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	const name = "credentials.json"
	path := filepath.Join(dirPath, name)
	if err := os.WriteFile(path, []byte(`{"token":"delete me"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Remove0600At(dir, name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normal candidate still exists: %v", err)
	}
	if err := Remove0600At(dir, name); err != nil {
		t.Fatalf("idempotent removal failed: %v", err)
	}
}

func TestRemove0600AtDoesNotOverwriteQuarantineCollision(t *testing.T) {
	dirPath := privateTestDir(t)
	dir, err := OpenPrivateDir(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	const name = "credentials.json"
	path := filepath.Join(dirPath, name)
	if err := os.WriteFile(path, []byte(`{"token":"delete me"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ops := hostDeletionOps()
	renameNoReplace := ops.renameNoReplace
	collisionPath := ""
	ops.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		if oldName == name && collisionPath == "" {
			collisionPath = filepath.Join(dirPath, newName)
			if err := os.WriteFile(collisionPath, []byte("collision"), 0o600); err != nil {
				return err
			}
		}
		return renameNoReplace(oldDirFD, oldName, newDirFD, newName)
	}
	if err := remove0600AtWithOps(dir, name, ops); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(collisionPath)
	if err != nil {
		t.Fatalf("pre-existing quarantine entry was removed: %v", err)
	}
	if string(got) != "collision" {
		t.Fatalf("pre-existing quarantine entry=%q, want collision", got)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normal candidate still exists: %v", err)
	}
}

func TestRemove0600AtRestoreDoesNotOverwriteReappearedSource(t *testing.T) {
	dirPath := privateTestDir(t)
	dir, err := OpenPrivateDir(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	const name = "credentials.json"
	path := filepath.Join(dirPath, name)
	if err := os.WriteFile(path, []byte(`{"token":"original"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ops := hostDeletionOps()
	openat := ops.openat
	quarantinePath := ""
	ops.openat = func(dirfd int, candidate string, flags int, mode uint32) (int, error) {
		if candidate != name && quarantinePath == "" {
			quarantinePath = filepath.Join(dirPath, candidate)
			if err := os.Chmod(quarantinePath, 0o644); err != nil {
				return -1, err
			}
			if err := os.WriteFile(path, []byte(`{"token":"reappeared"}`), 0o600); err != nil {
				return -1, err
			}
		}
		return openat(dirfd, candidate, flags, mode)
	}
	if err := remove0600AtWithOps(dir, name, ops); err == nil {
		t.Fatal("expected isolated candidate validation error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"token":"reappeared"}` {
		t.Fatalf("reappeared source was overwritten: %q", got)
	}
	info, err := os.Stat(quarantinePath)
	if err != nil {
		t.Fatalf("unsafe quarantine was deleted: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("quarantine mode=%o, want unchanged 0644", got)
	}
}

func TestRemove0600AtUnlinkFailureRestoresSource(t *testing.T) {
	dirPath := privateTestDir(t)
	dir, err := OpenPrivateDir(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	const name = "credentials.json"
	path := filepath.Join(dirPath, name)
	if err := os.WriteFile(path, []byte(`{"token":"original"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ops := hostDeletionOps()
	ops.unlinkat = func(int, string) error { return syscall.EIO }
	if err := remove0600AtWithOps(dir, name, ops); err == nil {
		t.Fatal("expected unlink error")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("source was not restored: %v", err)
	}
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pku-drive-delete-") {
			t.Fatalf("quarantine entry remained after successful restore: %s", entry.Name())
		}
	}
}

func TestEnsurePrivateDirSets0700DespiteUmask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pku-drive-cli")
	old := syscall.Umask(0o777)
	defer syscall.Umask(old)
	dir, err := EnsurePrivateDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o700); got != want {
		t.Fatalf("mode = %o, want %o", got, want)
	}
}

func TestEnsurePrivateDirHonorsHostileUmaskForBroaderParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-parent", "pku-drive-cli")
	old := syscall.Umask(0o777)
	defer syscall.Umask(old)
	if dir, err := EnsurePrivateDir(path); err == nil {
		_ = dir.Close()
		t.Fatal("expected caller umask to prevent opening a newly created broader parent")
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o000); got != want {
		t.Fatalf("broader parent mode = %o, want caller-umask mode %o", got, want)
	}
}

func TestEnsurePrivateDirDoesNotRelaxConcurrentCreatorUmask(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	start := make(chan struct{})
	creatorDone := make(chan error, 1)
	go func() {
		<-start
		for i := 0; i < 200; i++ {
			path := filepath.Join(root, fmt.Sprintf("unrelated-%d", i))
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
			if err != nil {
				creatorDone <- err
				return
			}
			if err := f.Close(); err != nil {
				creatorDone <- err
				return
			}
			info, err := os.Stat(path)
			if err != nil {
				creatorDone <- err
				return
			}
			if info.Mode().Perm() != 0o600 {
				creatorDone <- fmt.Errorf("unrelated file mode = %o, want 600", info.Mode().Perm())
				return
			}
		}
		creatorDone <- nil
	}()
	close(start)
	for i := 0; i < 200; i++ {
		dir, err := EnsurePrivateDir(filepath.Join(parent, fmt.Sprintf("private-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if err := dir.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-creatorDone; err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryBoundChildCreationSurvivesParentLeafSwap(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "pku-drive-cli")
	stateDir, err := EnsurePrivateDir(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateDir.Close()
	original := filepath.Join(root, "original")
	if err := os.Rename(statePath, original); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(root, "redirect")
	if err := os.Mkdir(redirect, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(redirect, statePath); err != nil {
		t.Fatal(err)
	}
	uploads, err := EnsurePrivateDirAt(stateDir, "uploads")
	if err != nil {
		t.Fatal(err)
	}
	defer uploads.Close()
	if _, err := os.Stat(filepath.Join(original, "uploads")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(redirect, "uploads")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("redirect received uploads directory: %v", err)
	}
}

func TestPathJSONOperationsRejectNonPrivateParentDirectory(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "credentials.json")
	if err := WriteJSONAtomic(path, map[string]string{"token": "value"}); err == nil {
		t.Fatal("expected non-private parent rejection")
	}
	if err := os.WriteFile(path, []byte(`{"token":"value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := ReadJSON0600(path, &got); err == nil {
		t.Fatal("expected non-private parent rejection")
	}
	if err := Remove0600(path); err == nil {
		t.Fatal("expected non-private parent rejection")
	}
}
