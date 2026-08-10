package config_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/config"
)

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	return config.Paths{
		ConfigDir:       filepath.Join(root, "config", "pku-drive-cli"),
		ConfigFile:      filepath.Join(root, "config", "pku-drive-cli", "config.json"),
		CredentialsFile: filepath.Join(root, "config", "pku-drive-cli", "credentials.json"),
		CredentialLock:  filepath.Join(root, "config", "pku-drive-cli", "credentials.lock"),
		UploadStateDir:  filepath.Join(root, "state", "pku-drive-cli", "uploads"),
	}
}

func TestCredentialRoundTripIs0600(t *testing.T) {
	paths := testPaths(t)
	store := config.NewStore(paths)
	want := config.Credentials{Server: "https://disk.pku.edu.cn", ClientID: "client", ClientSecret: "secret", RefreshToken: "refresh"}
	if err := store.SaveCredentials(want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("mode = %o, want %o", got, want)
	}
	got, err := store.LoadCredentials()
	if err != nil || got.RefreshToken != "refresh" {
		t.Fatalf("got = %+v, err = %v", got, err)
	}
}

func TestLoadConfigDefaultsWhenConfigDoesNotExist(t *testing.T) {
	store := config.NewStore(testPaths(t))
	got, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != config.DefaultServer {
		t.Fatalf("server = %q, want %q", got.Server, config.DefaultServer)
	}
}

func TestMissingConfigRejectsSymlinkedConfigLeaf(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigDir), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, paths.ConfigDir); err != nil {
		t.Fatal(err)
	}
	if _, err := config.NewStore(paths).LoadConfig(); err == nil {
		t.Fatal("expected symlinked config leaf rejection")
	}
}

func TestSaveConfigNormalizesTrailingSlash(t *testing.T) {
	paths := testPaths(t)
	store := config.NewStore(paths)
	if err := store.SaveConfig(config.Config{Server: "https://disk.pku.edu.cn/"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "https://disk.pku.edu.cn" {
		t.Fatalf("server = %q, want normalized server", got.Server)
	}
}

func TestSaveConfigRejectsInsecureOrAmbiguousServer(t *testing.T) {
	store := config.NewStore(testPaths(t))
	for _, server := range []string{
		"http://disk.pku.edu.cn",
		"https://user@disk.pku.edu.cn",
		"https://disk.pku.edu.cn?next=x",
		"https://disk.pku.edu.cn#fragment",
		"https://:443",
		"/relative",
	} {
		if err := store.SaveConfig(config.Config{Server: server}); err == nil {
			t.Errorf("SaveConfig(%q) unexpectedly succeeded", server)
		}
	}
}

func TestMissingCredentialsDeleteRejectsSymlinkedConfigLeaf(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigDir), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, paths.ConfigDir); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(paths).DeleteCredentials(); err == nil {
		t.Fatal("expected symlinked config leaf rejection")
	}
}

func TestCredentialPathSymlinkIsRejected(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(paths.ConfigDir), "target.json")
	if err := os.WriteFile(target, []byte(`{"refresh_token":"stolen"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, paths.CredentialsFile); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(paths)
	if _, err := store.LoadCredentials(); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestDeleteCredentialsRemovesStoredSecret(t *testing.T) {
	paths := testPaths(t)
	store := config.NewStore(paths)
	if err := store.SaveCredentials(config.Credentials{Server: config.DefaultServer, RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteCredentials(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.CredentialsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential file still exists or stat failed: %v", err)
	}
}

func TestDeleteCredentialsRemovesCorruptPrivateSecret(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.CredentialsFile, []byte(`{"refresh_token":`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(paths)
	if err := store.DeleteCredentials(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.CredentialsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential file still exists or stat failed: %v", err)
	}
}

func TestSaveRejectsToolDirectoryWithGroupAccess(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(paths.ConfigDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(paths)
	if err := store.SaveConfig(config.Config{Server: config.DefaultServer}); err == nil {
		t.Fatal("expected tool directory permission error")
	}
}

func TestSaveRejectsToolDirectoryWithSpecialPermissionBits(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(paths.ConfigDir, 0o2700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetgid == 0 {
		t.Skip("filesystem does not preserve setgid bits for this user")
	}
	store := config.NewStore(paths)
	if err := store.SaveConfig(config.Config{Server: config.DefaultServer}); err == nil {
		t.Fatal("expected tool directory special permission error")
	}
}

func TestSaveRejectsSymlinkedToolDirectories(t *testing.T) {
	t.Run("config leaf", func(t *testing.T) {
		paths := testPaths(t)
		if err := os.MkdirAll(filepath.Dir(paths.ConfigDir), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "config-target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, paths.ConfigDir); err != nil {
			t.Fatal(err)
		}
		if err := config.NewStore(paths).SaveConfig(config.Config{Server: config.DefaultServer}); err == nil {
			t.Fatal("expected config leaf symlink rejection")
		}
	})

	t.Run("uploads leaf", func(t *testing.T) {
		paths := testPaths(t)
		stateToolDir := filepath.Dir(paths.UploadStateDir)
		if err := os.MkdirAll(stateToolDir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "uploads-target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, paths.UploadStateDir); err != nil {
			t.Fatal(err)
		}
		if err := config.NewStore(paths).SaveConfig(config.Config{Server: config.DefaultServer}); err == nil {
			t.Fatal("expected uploads leaf symlink rejection")
		}
	})
}

func TestSaveCreatesPrivateUploadStateDirectories(t *testing.T) {
	paths := testPaths(t)
	store := config.NewStore(paths)
	if err := store.SaveConfig(config.Config{Server: config.DefaultServer}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(paths.UploadStateDir), paths.UploadStateDir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := info.Mode().Perm(), os.FileMode(0o700); got != want {
			t.Fatalf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestSaveDoesNotChangeBroaderXDGDirectoryModes(t *testing.T) {
	paths := testPaths(t)
	configRoot := filepath.Dir(paths.ConfigDir)
	stateRoot := filepath.Dir(filepath.Dir(paths.UploadStateDir))
	for _, path := range []string{configRoot, stateRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.NewStore(paths).SaveConfig(config.Config{Server: config.DefaultServer}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{configRoot, stateRoot} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := info.Mode().Perm(), os.FileMode(0o755); got != want {
			t.Fatalf("%s mode = %o, want unchanged %o", path, got, want)
		}
	}
}

func TestWithCredentialLockUsesPrivateLock(t *testing.T) {
	paths := testPaths(t)
	store := config.NewStore(paths)
	called := false
	if err := store.WithCredentialLock(context.Background(), func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("lock callback was not called")
	}
}

func TestStoreRejectsNonCanonicalDirectChildPaths(t *testing.T) {
	type operation struct {
		name string
		set  func(*config.Paths, string)
		run  func(*config.Store) error
	}
	operations := []operation{
		{
			name: "config",
			set:  func(paths *config.Paths, path string) { paths.ConfigFile = path },
			run: func(store *config.Store) error {
				return store.SaveConfig(config.Config{Server: config.DefaultServer})
			},
		},
		{
			name: "credentials",
			set:  func(paths *config.Paths, path string) { paths.CredentialsFile = path },
			run: func(store *config.Store) error {
				return store.SaveCredentials(config.Credentials{Server: config.DefaultServer})
			},
		},
		{
			name: "lock",
			set:  func(paths *config.Paths, path string) { paths.CredentialLock = path },
			run: func(store *config.Store) error {
				return store.WithCredentialLock(context.Background(), func() error { return nil })
			},
		},
	}
	variants := []struct {
		name  string
		build func(dir, child string) string
		valid bool
	}{
		{name: "valid direct child", build: func(dir, child string) string { return dir + "/" + child }, valid: true},
		{name: "trailing separator", build: func(dir, child string) string { return dir + "/" + child + "/" }},
		{name: "repeated separator", build: func(dir, child string) string { return dir + "//" + child }},
		{name: "dot component", build: func(dir, child string) string { return dir + "/./" + child }},
		{name: "dotdot component", build: func(dir, child string) string { return dir + "/nested/../" + child }},
		{name: "nested child", build: func(dir, child string) string { return dir + "/nested/" + child }},
	}
	for _, operation := range operations {
		for _, variant := range variants {
			t.Run(operation.name+"/"+variant.name, func(t *testing.T) {
				paths := testPaths(t)
				child := operation.name + ".json"
				if operation.name == "lock" {
					child = "credentials.lock"
				}
				operation.set(&paths, variant.build(paths.ConfigDir, child))
				err := operation.run(config.NewStore(paths))
				if variant.valid && err != nil {
					t.Fatalf("valid direct child failed: %v", err)
				}
				if !variant.valid && err == nil {
					t.Fatal("expected non-canonical child path rejection")
				}
			})
		}
	}
}

func TestStoreRejectsMalformedPathsBeforeFilesystemAccess(t *testing.T) {
	mutate := func(paths *config.Paths) {
		paths.ConfigFile = paths.ConfigDir + "/config.json/"
	}
	tests := []struct {
		name string
		set  func(*config.Paths)
	}{
		{name: "config", set: mutate},
		{name: "credentials", set: func(paths *config.Paths) { paths.CredentialsFile = paths.ConfigDir + "//credentials.json" }},
		{name: "lock", set: func(paths *config.Paths) { paths.CredentialLock = paths.ConfigDir + "/./credentials.lock" }},
		{name: "uploads", set: func(paths *config.Paths) { paths.UploadStateDir += "/" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := testPaths(t)
			test.set(&paths)
			store := config.NewStore(paths)
			if _, err := store.LoadConfig(); err == nil {
				t.Fatal("expected malformed path error before missing-config default")
			}
			if err := store.SaveConfig(config.Config{Server: config.DefaultServer}); err == nil {
				t.Fatal("expected malformed path error before save")
			}
			if _, err := os.Lstat(paths.ConfigDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("config leaf was created despite malformed path: %v", err)
			}
		})
	}
}
