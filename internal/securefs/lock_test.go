package securefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWithLockHonorsCanceledContextAndAllowsSequentialHolders(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "credentials.lock")
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- WithLock(context.Background(), path, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first lock holder did not start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() { secondDone <- WithLock(ctx, path, func() error { return nil }) }()
	select {
	case err := <-secondDone:
		t.Fatalf("contending lock returned before cancellation: %v", err)
	case <-time.After(35 * time.Millisecond):
	}
	cancel()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}

	called := false
	if err := WithLock(context.Background(), path, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("second sequential holder did not run")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("mode = %o, want %o", got, want)
	}
}

func TestDirectoryBoundLockContinuesContendingAfterLeafSwap(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "pku-drive-cli")
	dir, err := EnsurePrivateDir(leaf)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- WithLockAt(context.Background(), dir, "credentials.lock", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	original := filepath.Join(root, "original")
	if err := os.Rename(leaf, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- WithLockAt(context.Background(), dir, "credentials.lock", func() error { return nil })
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second holder bypassed retained-directory lock: %v", err)
	case <-time.After(35 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestWithLockRejectsSymlink(t *testing.T) {
	dir := privateTestDir(t)
	target := filepath.Join(dir, "target.lock")
	path := filepath.Join(dir, "credentials.lock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := WithLock(context.Background(), path, func() error { return nil }); err == nil {
		t.Fatal("expected symlink error")
	}
}

func TestWithLockRejectsUnsafeExistingEntries(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "directory",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wide mode",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "special bits",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
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
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(privateTestDir(t), "credentials.lock")
			test.setup(t, path)
			if err := WithLock(context.Background(), path, func() error { return nil }); err == nil {
				t.Fatal("expected unsafe lock entry rejection")
			}
		})
	}
}

func TestWithLockCreatesExact0600DespiteUmask(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "credentials.lock")
	old := syscall.Umask(0o777)
	defer syscall.Umask(old)
	if err := WithLock(context.Background(), path, func() error { return nil }); err != nil {
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

func TestWithLockRejectsNonPrivateParentDirectory(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WithLock(context.Background(), filepath.Join(parent, "credentials.lock"), func() error { return nil }); err == nil {
		t.Fatal("expected non-private parent rejection")
	}
}
