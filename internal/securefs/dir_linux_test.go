//go:build linux

package securefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestComponentWalkRejectsSymlinkAtEveryControlledComponent(t *testing.T) {
	for symlinkIndex := 0; symlinkIndex < 3; symlinkIndex++ {
		t.Run(fmt.Sprintf("component-%d", symlinkIndex), func(t *testing.T) {
			root := t.TempDir()
			target := t.TempDir()
			components := []string{"first", "second", "pku-drive-cli"}
			prefix := root
			for index, component := range components {
				path := filepath.Join(prefix, component)
				if index == symlinkIndex {
					targetPath := target
					for _, suffix := range components[index+1:] {
						targetPath = filepath.Join(targetPath, suffix)
						if err := os.Mkdir(targetPath, 0o700); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
					break
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				prefix = path
			}
			if dir, err := OpenPrivateDir(filepath.Join(root, filepath.Join(components...))); err == nil {
				_ = dir.Close()
				t.Fatal("expected component symlink rejection")
			}
		})
	}
}

func TestPrivateDirectoryRejectsUnsafeExistingEntries(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "regular file",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "mode 0755",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "mode 0000",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "special bits",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o2700); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode()&os.ModeSetgid == 0 {
					t.Skip("filesystem does not preserve setgid bits for this user")
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pku-drive-cli")
			test.setup(t, path)
			beforeInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			before := beforeInfo.Sys().(*syscall.Stat_t).Mode & 0o7777
			if dir, err := EnsurePrivateDir(path); err == nil {
				_ = dir.Close()
				t.Fatal("expected unsafe existing directory rejection")
			}
			afterInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			after := afterInfo.Sys().(*syscall.Stat_t).Mode & 0o7777
			if after != before {
				t.Fatalf("existing entry mode changed from %o to %o", before, after)
			}
		})
	}
}

func TestPrivateDirectoryRejectsWrongOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test requires a non-root effective user")
	}
	info, err := os.Stat("/root")
	if err != nil {
		t.Skipf("root-owned private directory unavailable: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) == os.Geteuid() || info.Mode().Perm() != 0o700 {
		t.Skip("host does not expose /root as another user's 0700 directory")
	}
	if dir, err := OpenPrivateDir("/root"); err == nil {
		_ = dir.Close()
		t.Fatal("expected wrong-owner directory rejection")
	}
}

func TestPrivateDirectoryValidationRejectsWrongUID(t *testing.T) {
	stat := syscall.Stat_t{
		Mode: syscall.S_IFDIR | 0o700,
		Uid:  uint32(os.Geteuid() + 1),
	}
	if err := validateDirectoryStat("pku-drive-cli", &stat, 0o700); err == nil {
		t.Fatal("expected wrong-UID directory rejection")
	}
}

func TestEnsurePrivateDirConcurrentInitializersStaySafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pku-drive-cli")
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			dir, err := EnsurePrivateDir(path)
			if err == nil {
				err = dir.Close()
			}
			results <- err
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes == 0 {
		t.Fatal("both concurrent initializers failed")
	}
	dir, err := OpenPrivateDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
}

func TestDirectoryCloseIsIdempotent(t *testing.T) {
	dir, err := openDirectoryTree(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryCloseErrorDoesNotCloseReusedDescriptor(t *testing.T) {
	ops := hostLinuxOps
	replacementFD := -1
	injected := false
	ops.close = func(fd int) error {
		if injected {
			return syscall.Close(fd)
		}
		injected = true
		if err := syscall.Close(fd); err != nil {
			return err
		}
		replacement, err := syscall.Open("/dev/null", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		if replacement != fd {
			if err := syscall.Dup2(replacement, fd); err != nil {
				_ = syscall.Close(replacement)
				return err
			}
			_ = syscall.Close(replacement)
		}
		replacementFD = fd
		return syscall.EIO
	}

	if dir, err := walkDirWithOps("/", false, ops); err == nil {
		_ = dir.Close()
		t.Fatal("expected injected close error")
	}
	if replacementFD < 0 {
		t.Fatal("close error was not injected")
	}
	defer syscall.Close(replacementFD)
	var stat syscall.Stat_t
	if err := syscall.Fstat(replacementFD, &stat); err != nil {
		t.Fatalf("reused descriptor was closed by deferred cleanup: %v", err)
	}
}

func TestEnsurePrivateDirAtRejectsRootSeparatorAsEntryName(t *testing.T) {
	if err := validateEntryName(string(filepath.Separator)); err == nil {
		t.Fatal("root separator was accepted as a descriptor-relative entry name")
	}
}

func TestComponentWalkRemainsBoundAfterIntermediateSwap(t *testing.T) {
	root := t.TempDir()
	anchor := filepath.Join(root, "anchor")
	nested := filepath.Join(anchor, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, "original")
	redirect := filepath.Join(root, "redirect")

	ops := hostLinuxOps
	openat := ops.openat
	swapped := false
	ops.openat = func(dirfd int, name string, flags int, mode uint32) (int, error) {
		fd, err := openat(dirfd, name, flags, mode)
		if err != nil || name != "anchor" || swapped {
			return fd, err
		}
		if err := os.Rename(anchor, original); err != nil {
			_ = syscall.Close(fd)
			return -1, err
		}
		if err := os.MkdirAll(filepath.Join(redirect, "nested"), 0o755); err != nil {
			_ = syscall.Close(fd)
			return -1, err
		}
		if err := os.Symlink(redirect, anchor); err != nil {
			_ = syscall.Close(fd)
			return -1, err
		}
		swapped = true
		return fd, nil
	}

	dir, err := walkDirWithOps(nested, false, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	uploads, err := EnsurePrivateDirAt(dir, "uploads")
	if err != nil {
		t.Fatal(err)
	}
	defer uploads.Close()
	if _, err := os.Stat(filepath.Join(original, "nested", "uploads")); err != nil {
		t.Fatalf("retained directory did not receive uploads: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(redirect, "nested", "uploads")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("redirect received uploads: %v", err)
	}
}

func TestPrivateDirectoryPromotionFailureStaysFailClosed(t *testing.T) {
	for _, injected := range []error{syscall.ENOSYS, syscall.EOPNOTSUPP} {
		t.Run(injected.Error(), func(t *testing.T) {
			parentPath := t.TempDir()
			parent, err := openDirectoryTree(parentPath, false)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()

			ops := hostLinuxOps
			ops.fchmodat = func(int, string, uint32, int) error { return injected }
			_, err = ensurePrivateDirAtWithOps(parent, "pku-drive-cli", ops)
			if err == nil {
				t.Fatal("expected Linux 6.6 compatibility error")
			}
			if !strings.Contains(err.Error(), "Linux 6.6") {
				t.Fatalf("error %q does not name the Linux 6.6 floor", err)
			}
			info, err := os.Stat(filepath.Join(parentPath, "pku-drive-cli"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0 {
				t.Fatalf("mode=%o, want 0000", info.Mode().Perm())
			}
			if _, err := EnsurePrivateDirAt(parent, "pku-drive-cli"); err == nil {
				t.Fatal("retry accepted an incomplete directory")
			}
		})
	}
}
