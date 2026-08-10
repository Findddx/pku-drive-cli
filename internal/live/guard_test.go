package live_test

import (
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/live"
)

func TestValidateManifest(t *testing.T) {
	good := live.Manifest{
		Nonce:       "0123456789abcdef0123456789abcdef",
		LibraryPath: "/个人文档",
		LibraryID:   "gns://library",
		TestPath:    "/个人文档/codex-smoke-0123456789abcdef0123456789abcdef",
		TestID:      "gns://child",
		SmallSize:   54,
		LargeSize:   134217728,
	}
	if err := live.ValidateManifest(good); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	bad := []struct {
		name     string
		manifest live.Manifest
	}{
		{name: "short nonce", manifest: withManifest(good, func(m *live.Manifest) { m.Nonce = "short" })},
		{name: "uppercase nonce", manifest: withManifest(good, func(m *live.Manifest) { m.Nonce = "0123456789ABCDEF0123456789abcdef" })},
		{name: "root is not a library", manifest: withManifest(good, func(m *live.Manifest) { m.LibraryPath = "/" })},
		{name: "non-normalized library", manifest: withManifest(good, func(m *live.Manifest) { m.LibraryPath = "/个人文档/../其他" })},
		{name: "wrong library", manifest: withManifest(good, func(m *live.Manifest) { m.LibraryPath = "/其他文档" })},
		{name: "path outside library", manifest: withManifest(good, func(m *live.Manifest) { m.TestPath = "/其他文档/codex-smoke-" + good.Nonce })},
		{name: "nested beneath exact directory", manifest: withManifest(good, func(m *live.Manifest) { m.TestPath += "/extra" })},
		{name: "non-normalized test path", manifest: withManifest(good, func(m *live.Manifest) { m.TestPath = "/个人文档//codex-smoke-" + good.Nonce })},
		{name: "wrong nonce in path", manifest: withManifest(good, func(m *live.Manifest) { m.TestPath = "/个人文档/codex-smoke-fedcba9876543210fedcba9876543210" })},
		{name: "invalid library id", manifest: withManifest(good, func(m *live.Manifest) { m.LibraryID = "not-gns" })},
		{name: "empty library id", manifest: withManifest(good, func(m *live.Manifest) { m.LibraryID = "gns://" })},
		{name: "invalid test id", manifest: withManifest(good, func(m *live.Manifest) { m.TestID = "not-gns" })},
		{name: "empty test id", manifest: withManifest(good, func(m *live.Manifest) { m.TestID = "gns://" })},
		{name: "test id equals library id", manifest: withManifest(good, func(m *live.Manifest) { m.TestID = m.LibraryID })},
		{name: "negative small size", manifest: withManifest(good, func(m *live.Manifest) { m.SmallSize = -1 })},
		{name: "wrong large size", manifest: withManifest(good, func(m *live.Manifest) { m.LargeSize = 1 })},
		{name: "invalid small id", manifest: withManifest(good, func(m *live.Manifest) { m.SmallID = "not-gns" })},
		{name: "file id equals directory", manifest: withManifest(good, func(m *live.Manifest) { m.SmallID = m.TestID })},
		{name: "duplicate file ids", manifest: withManifest(good, func(m *live.Manifest) { m.SmallID, m.LargeID = "gns://same", "gns://same" })},
	}
	for _, test := range bad {
		t.Run(test.name, func(t *testing.T) {
			if err := live.ValidateManifest(test.manifest); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
		})
	}
}

func TestValidateManifestRejectsMultiSegmentLibraryRoot(t *testing.T) {
	nonce := "0123456789abcdef0123456789abcdef"
	manifest := live.Manifest{
		Nonce:       nonce,
		LibraryPath: "/团队空间/我的入口",
		LibraryID:   "gns://library",
		TestPath:    "/团队空间/我的入口/codex-smoke-" + nonce,
		TestID:      "gns://child",
		SmallSize:   54,
		LargeSize:   134217728,
	}
	if err := live.ValidateManifest(manifest); err == nil {
		t.Fatal("multi-segment library root accepted")
	}
}

func TestValidateCleanupEntriesAllowsRecordedSubset(t *testing.T) {
	manifest := cleanupManifest()
	manifest.SmallID = "gns://small"
	manifest.LargeID = "gns://large"

	cases := []struct {
		name    string
		entries []anyshare.Item
	}{
		{name: "empty after partial failure"},
		{name: "small only", entries: []anyshare.Item{{
			ID: "gns://small", Name: "小文件.txt", Type: "file",
			Path: manifest.TestPath + "/小文件.txt", Size: manifest.SmallSize,
		}}},
		{name: "large only", entries: []anyshare.Item{{
			ID: "gns://large", Name: "multipart-128MiB.bin", Type: "file",
			Path: manifest.TestPath + "/multipart-128MiB.bin", Size: manifest.LargeSize,
		}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := live.ValidateCleanupEntries(manifest, test.entries); err != nil {
				t.Fatalf("safe subset rejected: %v", err)
			}
		})
	}
}

func TestValidateCleanupEntriesRejectsUnexpectedContents(t *testing.T) {
	manifest := cleanupManifest()
	manifest.SmallID = "gns://small"
	manifest.LargeID = "gns://large"
	small := anyshare.Item{
		ID: "gns://small", Name: "小文件.txt", Type: "file",
		Path: manifest.TestPath + "/小文件.txt", Size: manifest.SmallSize,
	}

	bad := []struct {
		name     string
		entries  []anyshare.Item
		manifest live.Manifest
	}{
		{name: "unknown name", entries: []anyshare.Item{{ID: "gns://other", Name: "other.txt", Type: "file", Path: manifest.TestPath + "/other.txt", Size: 1}}},
		{name: "subdirectory", entries: []anyshare.Item{{ID: "gns://small", Name: "小文件.txt", Type: "directory", Path: small.Path, Size: small.Size}}},
		{name: "wrong path", entries: []anyshare.Item{{ID: small.ID, Name: small.Name, Type: small.Type, Path: "/其他/小文件.txt", Size: small.Size}}},
		{name: "wrong size", entries: []anyshare.Item{{ID: small.ID, Name: small.Name, Type: small.Type, Path: small.Path, Size: small.Size + 1}}},
		{name: "wrong id", entries: []anyshare.Item{{ID: "gns://other", Name: small.Name, Type: small.Type, Path: small.Path, Size: small.Size}}},
		{name: "unrecorded id", entries: []anyshare.Item{{ID: small.ID, Name: small.Name, Type: small.Type, Path: small.Path, Size: small.Size}}, manifest: func() live.Manifest { m := manifest; m.SmallID = ""; return m }()},
		{name: "duplicate name", entries: []anyshare.Item{small, small}},
	}
	for _, test := range bad {
		t.Run(test.name, func(t *testing.T) {
			m := manifest
			if test.manifest.Nonce != "" {
				m = test.manifest
			}
			if err := live.ValidateCleanupEntries(m, test.entries); err == nil {
				t.Fatal("unsafe directory contents accepted")
			}
		})
	}
}

func TestValidateDeleted(t *testing.T) {
	manifest := cleanupManifest()
	if err := live.ValidateDeleted(manifest, nil); err != nil {
		t.Fatalf("absent directory rejected: %v", err)
	}
	bad := []struct {
		name  string
		entry anyshare.Item
	}{
		{name: "same id", entry: anyshare.Item{ID: manifest.TestID, Name: "other", Path: "/个人文档/other"}},
		{name: "same name replacement", entry: anyshare.Item{ID: "gns://replacement", Name: "codex-smoke-" + manifest.Nonce, Path: manifest.TestPath}},
		{name: "same path replacement", entry: anyshare.Item{ID: "gns://replacement", Name: "other", Path: manifest.TestPath}},
	}
	for _, test := range bad {
		t.Run(test.name, func(t *testing.T) {
			if err := live.ValidateDeleted(manifest, []anyshare.Item{test.entry}); err == nil {
				t.Fatal("present or replacement directory accepted as deleted")
			}
		})
	}
}

func cleanupManifest() live.Manifest {
	nonce := "0123456789abcdef0123456789abcdef"
	return live.Manifest{
		Nonce: nonce, LibraryPath: "/个人文档", LibraryID: "gns://library",
		TestPath: "/个人文档/codex-smoke-" + nonce, TestID: "gns://child",
		SmallSize: 54, LargeSize: 134217728,
	}
}

func withManifest(base live.Manifest, change func(*live.Manifest)) live.Manifest {
	change(&base)
	return base
}
