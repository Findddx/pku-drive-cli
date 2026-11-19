//go:build linux && amd64

package download

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

const filesystemMagicEXT4 = 0xEF53

// statfsModeLyingOps returns host operations wrapped so that statfs reports the
// given filesystem magic while fstat reports fixed 0755 permission bits for
// every entry, mirroring fuseblk (ntfs-3g) behaviour: creation modes and chmod
// are ignored and a default 0755 is presented instead.
func statfsModeLyingOps(magic int64) localLinuxOps {
	ops := hostLocalLinuxOps
	ops.statfs = func(fd int, buf *syscall.Statfs_t) error {
		*buf = syscall.Statfs_t{}
		buf.Type = magic
		return nil
	}
	hostFstat := ops.fstat
	ops.fstat = func(fd int, stat *syscall.Stat_t) error {
		if err := hostFstat(fd, stat); err != nil {
			return err
		}
		stat.Mode = stat.Mode&^0o7777 | 0o755
		return nil
	}
	return ops
}

func forceNamedTemporaryFallback(ops localLinuxOps) localLinuxOps {
	hostOpenat := ops.openat
	ops.openat = func(dirfd int, name string, flags int, mode uint32) (int, error) {
		if flags&oTmpfileLinux == oTmpfileLinux {
			return -1, syscall.EOPNOTSUPP
		}
		return hostOpenat(dirfd, name, flags, mode)
	}
	return ops
}

func TestValidTemporaryStatModePolicy(t *testing.T) {
	base := func() syscall.Stat_t {
		return syscall.Stat_t{
			Mode:  syscall.S_IFREG | 0o600,
			Uid:   uint32(os.Geteuid()),
			Nlink: 1,
			Size:  42,
		}
	}
	cases := []struct {
		name         string
		mutate       func(*syscall.Stat_t)
		modeEnforced bool
		want         bool
	}{
		{name: "mode 0600 enforced filesystem accepted", modeEnforced: true, want: true},
		{
			name:         "mode 0755 rejected on mode-trusting filesystem",
			mutate:       func(s *syscall.Stat_t) { s.Mode = syscall.S_IFREG | 0o755 },
			modeEnforced: true,
			want:         false,
		},
		{
			name:         "mode 0755 accepted on mode-ignoring filesystem",
			mutate:       func(s *syscall.Stat_t) { s.Mode = syscall.S_IFREG | 0o755 },
			modeEnforced: false,
			want:         true,
		},
		{
			name:         "mode 0600 accepted on mode-ignoring filesystem",
			modeEnforced: false,
			want:         true,
		},
		{
			name:         "non-regular rejected even when relaxed",
			mutate:       func(s *syscall.Stat_t) { s.Mode = syscall.S_IFDIR | 0o755 },
			modeEnforced: false,
			want:         false,
		},
		{
			name:         "foreign owner rejected even when relaxed",
			mutate:       func(s *syscall.Stat_t) { s.Uid = uint32(os.Geteuid()) + 1 },
			modeEnforced: false,
			want:         false,
		},
		{
			name:         "unexpected link count rejected even when relaxed",
			mutate:       func(s *syscall.Stat_t) { s.Nlink = 2 },
			modeEnforced: false,
			want:         false,
		},
		{
			name:         "unexpected size rejected even when relaxed",
			mutate:       func(s *syscall.Stat_t) { s.Size = 43 },
			modeEnforced: false,
			want:         false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stat := base()
			if tc.mutate != nil {
				tc.mutate(&stat)
			}
			if got := validTemporaryStat(&stat, 1, 42, tc.modeEnforced); got != tc.want {
				t.Fatalf("validTemporaryStat(enforced=%v) = %v, want %v", tc.modeEnforced, got, tc.want)
			}
		})
	}
}

func TestFuseblkTemporaryWithUntrustworthyModePassesValidation(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "result.bin")
	ops := forceNamedTemporaryFallback(statfsModeLyingOps(int64(filesystemMagicFUSEBLK)))
	target, err := openDownloadTargetWithOps(destination, false, ops, bytes.NewReader(make([]byte, 128)))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if target.modeEnforced {
		t.Fatal("fuseblk target must disable the mode 0600 assertion")
	}
	temporary := writeTemporary(t, target, []byte("download"))
	defer temporary.Close()
	if temporary.namedOrigin == "" {
		t.Fatal("test did not exercise named O_EXCL fallback")
	}
	if err := target.Install(temporary, int64(len("download"))); err != nil {
		t.Fatalf("install on mode-ignoring filesystem failed: %v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "download" {
		t.Fatalf("destination=%q err=%v", data, err)
	}
}

func TestModeTrustingFilesystemStillRejectsNonPrivateTemporaryMode(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "result.bin")
	ops := statfsModeLyingOps(int64(filesystemMagicEXT4))
	target, err := openDownloadTargetWithOps(destination, false, ops, bytes.NewReader(make([]byte, 128)))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if !target.modeEnforced {
		t.Fatal("ext4-backed target must enforce the mode 0600 assertion")
	}
	if _, err := target.CreateTemp(); !errors.Is(err, errUnsafeLocalEntry) {
		t.Fatalf("create temp err=%v, want unsafe local entry", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination was published: %v", err)
	}
	assertNoDownloadStagingEntries(t, directory)
}

func TestRelaxedModeStillDetectsTemporaryLinkCountChange(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "result.bin")
	ops := forceNamedTemporaryFallback(statfsModeLyingOps(int64(filesystemMagicFUSEBLK)))
	target, err := openDownloadTargetWithOps(destination, false, ops, bytes.NewReader(make([]byte, 128)))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	temporary := writeTemporary(t, target, []byte("download"))
	defer temporary.Close()
	if err := os.Link(filepath.Join(directory, temporary.namedOrigin), filepath.Join(directory, "extra-link")); err != nil {
		t.Fatal(err)
	}
	if err := temporary.ValidateForWrite(); !errors.Is(err, errTemporaryChanged) {
		t.Fatalf("validate err=%v, want temporary changed", err)
	}
	if err := target.Install(temporary, int64(len("download"))); !errors.Is(err, errTemporaryChanged) {
		t.Fatalf("install err=%v, want temporary changed", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination was published after link count change: %v", err)
	}
}
