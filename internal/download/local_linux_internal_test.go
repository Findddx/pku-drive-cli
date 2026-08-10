//go:build linux && amd64

package download

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLocalTargetRemainsBoundAfterParentRenameAndReplacement(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "destination")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := openDownloadTarget(filepath.Join(parent, "result.bin"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	original := filepath.Join(root, "original")
	if err := os.Rename(parent, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	temporary := writeTemporary(t, target, []byte("descriptor-bound"))
	defer temporary.Close()
	if err := target.Install(temporary, int64(len("descriptor-bound"))); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(original, "result.bin"))
	if err != nil || string(data) != "descriptor-bound" {
		t.Fatalf("bound destination=%q err=%v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(parent, "result.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement parent received download: %v", err)
	}
}

func TestLocalTargetNoReplacePreservesRacingDestination(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "result.bin")
	target, err := openDownloadTarget(destination, false)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	temporary := writeTemporary(t, target, []byte("download"))
	defer temporary.Close()
	if err := os.WriteFile(destination, []byte("racer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := target.Install(temporary, int64(len("download"))); !errors.Is(err, errDestinationExists) {
		t.Fatalf("install err=%v, want destination exists", err)
	}
	data, _ := os.ReadFile(destination)
	if string(data) != "racer" {
		t.Fatalf("racing destination changed to %q", data)
	}
}

func TestLocalTargetOverwriteRejectsDestinationReplacement(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "result.bin")
	if err := os.WriteFile(destination, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := openDownloadTarget(destination, true)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	temporary := writeTemporary(t, target, []byte("download"))
	defer temporary.Close()
	racer := filepath.Join(directory, "racer")
	if err := os.WriteFile(racer, []byte("racer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(racer, destination); err != nil {
		t.Fatal(err)
	}
	if err := target.Install(temporary, int64(len("download"))); !errors.Is(err, errDestinationChanged) {
		t.Fatalf("install err=%v, want destination changed", err)
	}
	data, _ := os.ReadFile(destination)
	if string(data) != "racer" {
		t.Fatalf("racing destination changed to %q", data)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoDownloadStagingEntries(t, directory)
}

func TestLocalTargetDoesNotRollbackAcrossUnknownStagingReplacement(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "result.bin")
	if err := os.WriteFile(destination, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := hostLocalLinuxOps
	hostExchange := ops.renameExchange
	exchanges := 0
	ops.renameExchange = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		exchanges++
		if err := hostExchange(oldDirFD, oldName, newDirFD, newName); err != nil {
			return err
		}
		if exchanges == 1 {
			if err := os.Rename(filepath.Join(directory, oldName), filepath.Join(directory, "stolen-original")); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(directory, oldName), []byte("attacker"), 0o600); err != nil {
				return err
			}
		}
		return nil
	}
	target, err := openDownloadTargetWithOps(destination, true, ops, bytes.NewReader(make([]byte, 64)))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	temporary := writeTemporary(t, target, []byte("download"))
	defer temporary.Close()
	if err := target.Install(temporary, int64(len("download"))); !errors.Is(err, errDestinationChanged) {
		t.Fatalf("install err=%v, want destination changed", err)
	}
	if exchanges != 1 {
		t.Fatalf("exchange calls=%d, unsafe rollback exchanged unknown entries", exchanges)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "attacker" {
		t.Fatalf("unknown staging replacement was moved into destination: %q", data)
	}
}

func TestCommittedTemporaryCloseErrorDoesNotReverseSuccess(t *testing.T) {
	directory := t.TempDir()
	target, err := openDownloadTarget(filepath.Join(directory, "result.bin"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	temporary := writeTemporary(t, target, []byte("download"))
	if err := target.Install(temporary, int64(len("download"))); err != nil {
		t.Fatal(err)
	}
	// Force os.File.Close to observe EBADF. The verified destination is already
	// published and directory-synced, so Close must not turn success into failure.
	if err := syscall.Close(int(temporary.file.Fd())); err != nil {
		t.Fatal(err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatalf("committed close error escaped: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "result.bin"))
	if err != nil || string(data) != "download" {
		t.Fatalf("destination=%q err=%v", data, err)
	}
}

func TestNamedTemporarySwapIsDetectedBeforeInstallation(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "result.bin")
	ops := hostLocalLinuxOps
	hostOpenat := ops.openat
	ops.openat = func(dirfd int, name string, flags int, mode uint32) (int, error) {
		if flags&oTmpfileLinux == oTmpfileLinux {
			return -1, syscall.EOPNOTSUPP
		}
		return hostOpenat(dirfd, name, flags, mode)
	}
	target, err := openDownloadTargetWithOps(destination, false, ops, bytes.NewReader(make([]byte, 64)))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	temporary := writeTemporary(t, target, []byte("download"))
	defer temporary.Close()
	if temporary.namedOrigin == "" {
		t.Fatal("test did not exercise named O_EXCL fallback")
	}
	origin := filepath.Join(directory, temporary.namedOrigin)
	stolen := filepath.Join(directory, "stolen-original-temp")
	if err := os.Rename(origin, stolen); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(origin, []byte("attacker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := target.Install(temporary, int64(len("download"))); !errors.Is(err, errTemporaryChanged) {
		t.Fatalf("install err=%v, want temporary changed", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination was published after temp swap: %v", err)
	}
	data, _ := os.ReadFile(origin)
	if string(data) != "attacker" {
		t.Fatalf("swapped entry was modified: %q", data)
	}
}

func TestMaximumLengthDestinationNameDoesNotExpandTemporaryName(t *testing.T) {
	directory := t.TempDir()
	name := strings.Repeat("a", 255)
	target, err := openDownloadTarget(filepath.Join(directory, name), false)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	temporary := writeTemporary(t, target, []byte("x"))
	defer temporary.Close()
	if err := target.Install(temporary, 1); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(directory, name)); err != nil || string(data) != "x" {
		t.Fatalf("destination=%q err=%v", data, err)
	}
}

func writeTemporary(t *testing.T, target *downloadTarget, data []byte) *downloadTemporary {
	t.Helper()
	temporary, err := target.CreateTemp()
	if err != nil {
		t.Fatal(err)
	}
	if err := temporary.ValidateForWrite(); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if _, err := temporary.File().Write(data); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	return temporary
}

func assertNoDownloadStagingEntries(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pku-drive-stage-") || strings.HasPrefix(entry.Name(), ".pku-drive-download-") {
			t.Fatalf("unexpected download staging entry %q", entry.Name())
		}
	}
}
