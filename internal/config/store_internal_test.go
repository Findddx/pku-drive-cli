package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/securefs"
)

func pathsBelow(root string) Paths {
	return Paths{
		ConfigDir:       filepath.Join(root, "config", "pku-drive-cli"),
		ConfigFile:      filepath.Join(root, "config", "pku-drive-cli", "config.json"),
		CredentialsFile: filepath.Join(root, "config", "pku-drive-cli", "credentials.json"),
		CredentialLock:  filepath.Join(root, "config", "pku-drive-cli", "credentials.lock"),
		UploadStateDir:  filepath.Join(root, "state", "pku-drive-cli", "uploads"),
	}
}

func TestInvalidPathsFailBeforeFilesystemAccess(t *testing.T) {
	root := t.TempDir()
	validRoot := filepath.Join(root, "not-created")
	valid := pathsBelow(validRoot)
	cases := []struct {
		name   string
		mutate func(*Paths)
	}{
		{"empty config dir", func(p *Paths) { p.ConfigDir = "" }},
		{"empty config", func(p *Paths) { p.ConfigFile = "" }},
		{"empty credentials", func(p *Paths) { p.CredentialsFile = "" }},
		{"empty lock", func(p *Paths) { p.CredentialLock = "" }},
		{"empty uploads", func(p *Paths) { p.UploadStateDir = "" }},
		{"nul config dir", func(p *Paths) { p.ConfigDir += "\x00" }},
		{"nul config", func(p *Paths) { p.ConfigFile += "\x00" }},
		{"nul credential", func(p *Paths) { p.CredentialsFile += "\x00" }},
		{"nul lock", func(p *Paths) { p.CredentialLock += "\x00" }},
		{"nul uploads", func(p *Paths) { p.UploadStateDir += "\x00" }},
		{"relative config dir", func(p *Paths) { p.ConfigDir = "relative/pku-drive-cli" }},
		{"relative config", func(p *Paths) { p.ConfigFile = "github.com/Findddx/pku-drive-cli/config.json" }},
		{"relative credentials", func(p *Paths) { p.CredentialsFile = "github.com/Findddx/pku-drive-cli/credentials.json" }},
		{"relative lock", func(p *Paths) { p.CredentialLock = "github.com/Findddx/pku-drive-cli/credentials.lock" }},
		{"relative uploads", func(p *Paths) { p.UploadStateDir = "github.com/Findddx/pku-drive-cli/uploads" }},
		{"trailing config dir", func(p *Paths) { p.ConfigDir += "/" }},
		{"trailing config", func(p *Paths) { p.ConfigFile += "/" }},
		{"trailing credentials", func(p *Paths) { p.CredentialsFile += "/" }},
		{"trailing lock", func(p *Paths) { p.CredentialLock += "/" }},
		{"trailing uploads", func(p *Paths) { p.UploadStateDir += "/" }},
		{"repeated config dir", func(p *Paths) { p.ConfigDir = filepath.Dir(p.ConfigDir) + "//pku-drive-cli" }},
		{"repeated config", func(p *Paths) { p.ConfigFile = p.ConfigDir + "//config.json" }},
		{"repeated credentials", func(p *Paths) { p.CredentialsFile = p.ConfigDir + "//credentials.json" }},
		{"repeated lock", func(p *Paths) { p.CredentialLock = p.ConfigDir + "//credentials.lock" }},
		{"repeated uploads", func(p *Paths) { p.UploadStateDir = filepath.Dir(p.UploadStateDir) + "//uploads" }},
		{"dot config dir", func(p *Paths) { p.ConfigDir += "/missing/.." }},
		{"dot config", func(p *Paths) { p.ConfigFile = p.ConfigDir + "/./config.json" }},
		{"dot credentials", func(p *Paths) { p.CredentialsFile = p.ConfigDir + "/./credentials.json" }},
		{"dot lock", func(p *Paths) { p.CredentialLock = p.ConfigDir + "/./credentials.lock" }},
		{"dot uploads", func(p *Paths) { p.UploadStateDir = filepath.Dir(p.UploadStateDir) + "/./uploads" }},
		{"dotdot config", func(p *Paths) { p.ConfigFile = p.ConfigDir + "/nested/../config.json" }},
		{"dotdot credentials", func(p *Paths) { p.CredentialsFile = p.ConfigDir + "/nested/../credentials.json" }},
		{"dotdot lock", func(p *Paths) { p.CredentialLock = p.ConfigDir + "/nested/../credentials.lock" }},
		{"dotdot uploads", func(p *Paths) { p.UploadStateDir = filepath.Dir(p.UploadStateDir) + "/nested/../uploads" }},
		{"nested config", func(p *Paths) { p.ConfigFile = filepath.Join(p.ConfigDir, "nested", "config.json") }},
		{"nested credentials", func(p *Paths) { p.CredentialsFile = filepath.Join(p.ConfigDir, "nested", "credentials.json") }},
		{"nested lock", func(p *Paths) { p.CredentialLock = filepath.Join(p.ConfigDir, "nested", "credentials.lock") }},
		{"nested uploads", func(p *Paths) { p.UploadStateDir = filepath.Join(filepath.Dir(p.UploadStateDir), "nested", "uploads") }},
		{"wrong config dir name", func(p *Paths) { p.ConfigDir = filepath.Join(filepath.Dir(p.ConfigDir), "other") }},
		{"wrong state dir name", func(p *Paths) {
			p.UploadStateDir = filepath.Join(filepath.Dir(filepath.Dir(p.UploadStateDir)), "other", "uploads")
		}},
		{"wrong config name", func(p *Paths) { p.ConfigFile = filepath.Join(p.ConfigDir, "other.json") }},
		{"wrong credentials name", func(p *Paths) { p.CredentialsFile = filepath.Join(p.ConfigDir, "other.json") }},
		{"wrong lock name", func(p *Paths) { p.CredentialLock = filepath.Join(p.ConfigDir, "other.lock") }},
		{"wrong uploads name", func(p *Paths) { p.UploadStateDir = filepath.Join(filepath.Dir(p.UploadStateDir), "other") }},
		{"config collision", func(p *Paths) { p.CredentialsFile = p.ConfigFile }},
		{"lock collision", func(p *Paths) { p.CredentialLock = p.ConfigFile }},
		{"upload collision", func(p *Paths) { p.UploadStateDir = p.ConfigFile }},
		{"config dir is config file", func(p *Paths) { p.ConfigDir = p.ConfigFile }},
		{"state dir overlaps config file", func(p *Paths) { p.UploadStateDir = filepath.Join(p.ConfigFile, "uploads") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := valid
			tc.mutate(&paths)
			store := NewStore(paths)
			calls := []func() error{
				func() error { _, err := store.LoadConfig(); return err },
				func() error { return store.SaveConfig(Config{Server: DefaultServer}) },
				func() error { _, err := store.LoadCredentials(); return err },
				func() error { return store.SaveCredentials(Credentials{Server: DefaultServer}) },
				store.DeleteCredentials,
				func() error {
					return store.WithCredentialLock(context.Background(), func() error { return nil })
				},
			}
			for i, call := range calls {
				if err := call(); err == nil {
					t.Fatalf("call %d accepted invalid paths", i)
				}
			}
			if _, err := os.Lstat(validRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid paths caused filesystem side effect: %v", err)
			}
		})
	}
}

type swappingDirs struct {
	statePath, original, redirect string
}

func (ops *swappingDirs) Ensure(path string) (*securefs.Dir, error) {
	dir, err := securefs.EnsurePrivateDir(path)
	if err != nil || path != ops.statePath {
		return dir, err
	}
	if err := os.Rename(path, ops.original); err != nil {
		_ = dir.Close()
		return nil, err
	}
	if err := os.Mkdir(ops.redirect, 0o700); err != nil {
		_ = dir.Close()
		return nil, err
	}
	if err := os.Symlink(ops.redirect, path); err != nil {
		_ = dir.Close()
		return nil, err
	}
	return dir, nil
}

func (ops *swappingDirs) EnsureAt(parent *securefs.Dir, name string) (*securefs.Dir, error) {
	return securefs.EnsurePrivateDirAt(parent, name)
}

func TestStoreCreatesUploadsThroughRetainedStateDir(t *testing.T) {
	root := t.TempDir()
	paths := Paths{
		ConfigDir:       filepath.Join(root, "config", "pku-drive-cli"),
		ConfigFile:      filepath.Join(root, "config", "pku-drive-cli", "config.json"),
		CredentialsFile: filepath.Join(root, "config", "pku-drive-cli", "credentials.json"),
		CredentialLock:  filepath.Join(root, "config", "pku-drive-cli", "credentials.lock"),
		UploadStateDir:  filepath.Join(root, "state", "pku-drive-cli", "uploads"),
	}
	statePath := filepath.Dir(paths.UploadStateDir)
	ops := &swappingDirs{
		statePath: statePath,
		original:  filepath.Join(filepath.Dir(statePath), "original"),
		redirect:  filepath.Join(filepath.Dir(statePath), "redirect"),
	}
	store := NewStore(paths)
	store.dirs = ops
	if err := store.SaveConfig(Config{Server: DefaultServer}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ops.original, "uploads")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ops.redirect, "uploads")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("redirect received uploads directory: %v", err)
	}
}
