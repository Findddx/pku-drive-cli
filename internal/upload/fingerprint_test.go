package upload_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/upload"
)

func TestOpenFingerprintKnownContentAndRewindsDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	file, got, err := upload.OpenFingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if got.Device == 0 || got.Inode == 0 || got.MtimeNS == 0 || got.Size != 5 || got.MD5 != "5D41402ABC4B2A76B9719D911017C592" || got.SliceMD5 != "5D41402ABC4B2A76B9719D911017C592" || got.CRC32 != "3610A686" {
		t.Fatalf("fingerprint=%+v", got)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "hello" {
		t.Fatalf("descriptor contents=%q, want hello", contents)
	}
}

func TestOpenFingerprintEmptyContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file, got, err := upload.OpenFingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got.Size != 0 || got.MD5 != "D41D8CD98F00B204E9800998ECF8427E" || got.SliceMD5 != "D41D8CD98F00B204E9800998ECF8427E" || got.CRC32 != "00000000" {
		t.Fatalf("fingerprint=%+v", got)
	}
}

func TestOpenFingerprintSliceStopsAt200KiB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large")
	contents := append(make([]byte, 200*1024), []byte("TAIL")...)
	for i := 0; i < 200*1024; i++ {
		contents[i] = 'a'
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	file, got, err := upload.OpenFingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got.Size != 204804 || got.MD5 != "03B0D570A43666BE20BA0BDFAD2A28EC" || got.SliceMD5 != "87803AC1CABD226EEB9C7665EFE2A758" || got.CRC32 != "50195F08" {
		t.Fatalf("fingerprint=%+v", got)
	}
}

func TestOpenFingerprintRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, _, err := upload.OpenFingerprint(link)
	assertCategory(t, err, apperr.Local)
}

func TestOpenFingerprintRejectsNonRegularFiles(t *testing.T) {
	for _, path := range []string{t.TempDir(), "/dev/null"} {
		file, _, err := upload.OpenFingerprint(path)
		if file != nil {
			file.Close()
			t.Fatalf("OpenFingerprint(%q) returned a descriptor", path)
		}
		assertCategory(t, err, apperr.Local)
	}
}

func TestFingerprintUnchangedUsesOpenedDescriptor(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, fingerprint, err := upload.OpenFingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	unchanged, err := fingerprint.Unchanged(file)
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged {
		t.Fatal("opened descriptor reported changed after pathname replacement")
	}
}

func TestFingerprintUnchangedDetectsMetadataMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, fingerprint, err := upload.OpenFingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString("!"); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	unchanged, err := fingerprint.Unchanged(file)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged {
		t.Fatal("size mutation reported unchanged")
	}

	mtime := time.Unix(123, 456)
	if err := os.Truncate(path, fingerprint.Size); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	unchanged, err = fingerprint.Unchanged(file)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged {
		t.Fatal("mtime mutation reported unchanged")
	}
}

func TestFingerprintUnchangedRejectsClosedDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, fingerprint, err := upload.OpenFingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = fingerprint.Unchanged(file)
	assertCategory(t, err, apperr.Local)
}

func TestOpenFingerprintErrorsDoNotLeakDescriptors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux procfs descriptor accounting")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	before := openFDCount(t)
	for range 64 {
		file, _, err := upload.OpenFingerprint(link)
		if file != nil {
			file.Close()
			t.Fatal("symlink error returned descriptor")
		}
		if err == nil {
			t.Fatal("symlink unexpectedly accepted")
		}
		file, _, err = upload.OpenFingerprint(root)
		if file != nil {
			file.Close()
			t.Fatal("directory error returned descriptor")
		}
		if err == nil {
			t.Fatal("directory unexpectedly accepted")
		}
	}
	after := openFDCount(t)
	if after != before {
		t.Fatalf("open descriptor count grew from %d to %d", before, after)
	}
}

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func assertCategory(t *testing.T, err error, want apperr.Category) {
	t.Helper()
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != want {
		t.Fatalf("err=%v, want category %q", err, want)
	}
}
