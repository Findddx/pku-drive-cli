package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPathsUsesExactXDGLocations(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "xdg-config")
	stateHome := filepath.Join(t.TempDir(), "xdg-state")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)

	got, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configHome, "pku-drive-cli"); got.ConfigDir != want {
		t.Fatalf("ConfigDir = %q, want %q", got.ConfigDir, want)
	}
	if want := filepath.Join(configHome, "pku-drive-cli", "config.json"); got.ConfigFile != want {
		t.Fatalf("ConfigFile = %q, want %q", got.ConfigFile, want)
	}
	if want := filepath.Join(configHome, "pku-drive-cli", "credentials.json"); got.CredentialsFile != want {
		t.Fatalf("CredentialsFile = %q, want %q", got.CredentialsFile, want)
	}
	if want := filepath.Join(configHome, "pku-drive-cli", "credentials.lock"); got.CredentialLock != want {
		t.Fatalf("CredentialLock = %q, want %q", got.CredentialLock, want)
	}
	if want := filepath.Join(stateHome, "pku-drive-cli", "uploads"); got.UploadStateDir != want {
		t.Fatalf("UploadStateDir = %q, want %q", got.UploadStateDir, want)
	}
}

func TestDefaultPathsUsesHomeFallbackLocations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	got, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "pku-drive-cli"); got.ConfigDir != want {
		t.Fatalf("ConfigDir = %q, want %q", got.ConfigDir, want)
	}
	if want := filepath.Join(home, ".local", "state", "pku-drive-cli", "uploads"); got.UploadStateDir != want {
		t.Fatalf("UploadStateDir = %q, want %q", got.UploadStateDir, want)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal(err)
	}
}
