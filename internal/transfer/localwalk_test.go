package transfer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

func TestWalkLocalFilesCollectsRegularTreeWithRelativePaths(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "数据"))
	write(t, filepath.Join(root, "a.csv"), 0o600)
	write(t, filepath.Join(root, "数据", "b.csv"), 0o600)
	mkdir(t, filepath.Join(root, "数据", "空")) // empty dir: no entries
	mkdir(t, filepath.Join(root, "zz"))

	files, skipped, err := walkLocalFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	wantRel := []string{"a.csv", "数据/b.csv"}
	gotRel := make([]string, len(files))
	for i, f := range files {
		gotRel[i] = f.Relative
		absWant, absErr := filepath.Abs(filepath.Join(root, filepath.FromSlash(f.Relative)))
		if absErr != nil || f.Absolute != absWant {
			t.Fatalf("absolute=%q want=%q err=%v", f.Absolute, absWant, absErr)
		}
	}
	if !reflect.DeepEqual(gotRel, wantRel) {
		t.Fatalf("relatives=%v want=%v", gotRel, wantRel)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped=%v", skipped)
	}
}

func TestWalkLocalFilesSkipsSymlinksWithoutFollowing(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	write(t, filepath.Join(target, "inner.csv"), 0o600)
	write(t, filepath.Join(root, "real.csv"), 0o600)
	if err := os.Symlink(filepath.Join(target, "inner.csv"), filepath.Join(root, "file-link.csv")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "dir-link")); err != nil {
		t.Fatal(err)
	}

	files, skipped, err := walkLocalFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Relative != "real.csv" {
		t.Fatalf("files=%+v", files)
	}
	want := []Skipped{{Path: "dir-link", Reason: SkipSymlink}, {Path: "file-link.csv", Reason: SkipSymlink}}
	if !reflect.DeepEqual(skipped, want) {
		t.Fatalf("skipped=%v want=%v", skipped, want)
	}
	// The symlinked tree content must not have been traversed.
	for _, f := range files {
		if f.Relative != "real.csv" {
			t.Fatalf("followed symlink into %q", f.Relative)
		}
	}
}

func TestWalkLocalFilesRootSymlinkIsLocalError(t *testing.T) {
	t.Run("root symlink to directory", func(t *testing.T) {
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		_, _, err := walkLocalFiles(link)
		assertLocalError(t, err)
	})
	t.Run("root symlink to file", func(t *testing.T) {
		target := t.TempDir()
		file := filepath.Join(target, "file")
		write(t, file, 0o600)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(file, link); err != nil {
			t.Fatal(err)
		}
		_, _, err := walkLocalFiles(link)
		assertLocalError(t, err)
	})
}

func TestWalkLocalFilesMissingRootIsLocalError(t *testing.T) {
	_, _, err := walkLocalFiles(filepath.Join(t.TempDir(), "does-not-exist"))
	assertLocalError(t, err)
}

func TestWalkLocalFilesSingleFileRootReturnsOneEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.bin")
	write(t, path, 0o600)
	files, skipped, err := walkLocalFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Relative != "single.bin" {
		t.Fatalf("files=%+v", files)
	}
	if !filepath.IsAbs(files[0].Absolute) {
		t.Fatalf("absolute=%q", files[0].Absolute)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped=%v", skipped)
	}
}

func TestClassifyLocalEntry(t *testing.T) {
	tests := []struct {
		name      string
		mode      fs.FileMode
		wantKind  localEntryKind
		wantError bool
	}{
		{"regular", 0o600, localEntryFile, false},
		{"directory", fs.ModeDir | 0o755, localEntryDirectory, false},
		{"symlink", fs.ModeSymlink | 0o777, localEntrySymlink, false},
		{"socket", fs.ModeSocket, localEntryUnsupported, true},
		{"named pipe", fs.ModeNamedPipe, localEntryUnsupported, true},
		{"char device", fs.ModeDevice | fs.ModeCharDevice, localEntryUnsupported, true},
		{"block device", fs.ModeDevice, localEntryUnsupported, true},
		{"irregular", fs.ModeIrregular, localEntryUnsupported, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, err := classifyLocalEntry(test.mode)
			if kind != test.wantKind {
				t.Fatalf("kind=%v want=%v", kind, test.wantKind)
			}
			if test.wantError && err == nil {
				t.Fatal("want local error")
			}
			if !test.wantError && err != nil {
				t.Fatalf("unexpected error=%v", err)
			}
			if err != nil {
				assertLocalError(t, err)
			}
		})
	}
}

func assertLocalError(t *testing.T, err error) {
	t.Helper()
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Local {
		t.Fatalf("err=%v want Local category", err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path string, mode os.FileMode) []byte {
	t.Helper()
	data := []byte("content of " + filepath.Base(path) + "\n")
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	return data
}
