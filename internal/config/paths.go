// Package config stores pku-drive-cli's local configuration and credentials.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Paths names the private files and directories owned by pku-drive-cli.
type Paths struct {
	ConfigDir       string
	ConfigFile      string
	CredentialsFile string
	CredentialLock  string
	UploadStateDir  string
}

type validatedPaths struct {
	configDir, configFile, credentialsFile, credentialLock string
	stateToolDir, uploadStateDir                           string
	configName, credentialsName, lockName, uploadsName     string
}

func validatePaths(paths Paths) (validatedPaths, error) {
	fields := []struct {
		name  string
		value string
	}{
		{"config directory", paths.ConfigDir},
		{"config file", paths.ConfigFile},
		{"credentials file", paths.CredentialsFile},
		{"credential lock", paths.CredentialLock},
		{"upload state directory", paths.UploadStateDir},
	}
	for _, field := range fields {
		if err := validateStoragePath(field.name, field.value); err != nil {
			return validatedPaths{}, err
		}
	}

	stateToolDir := filepath.Dir(paths.UploadStateDir)
	if filepath.Base(paths.ConfigDir) != "pku-drive-cli" {
		return validatedPaths{}, fmt.Errorf("unsafe config directory: must end in pku-drive-cli")
	}
	if filepath.Base(stateToolDir) != "pku-drive-cli" {
		return validatedPaths{}, fmt.Errorf("unsafe state directory: must end in pku-drive-cli")
	}
	if paths.ConfigFile != filepath.Join(paths.ConfigDir, "config.json") {
		return validatedPaths{}, fmt.Errorf("unsafe config file: must be config.json directly below config directory")
	}
	if paths.CredentialsFile != filepath.Join(paths.ConfigDir, "credentials.json") {
		return validatedPaths{}, fmt.Errorf("unsafe credentials file: must be credentials.json directly below config directory")
	}
	if paths.CredentialLock != filepath.Join(paths.ConfigDir, "credentials.lock") {
		return validatedPaths{}, fmt.Errorf("unsafe credential lock: must be credentials.lock directly below config directory")
	}
	if paths.UploadStateDir != filepath.Join(stateToolDir, "uploads") {
		return validatedPaths{}, fmt.Errorf("unsafe upload state directory: must be uploads directly below state directory")
	}
	if paths.ConfigDir == stateToolDir || pathContains(paths.ConfigDir, stateToolDir) || pathContains(stateToolDir, paths.ConfigDir) {
		return validatedPaths{}, fmt.Errorf("unsafe storage layout: config and state directory roles overlap")
	}

	return validatedPaths{
		configDir:       paths.ConfigDir,
		configFile:      paths.ConfigFile,
		credentialsFile: paths.CredentialsFile,
		credentialLock:  paths.CredentialLock,
		stateToolDir:    stateToolDir,
		uploadStateDir:  paths.UploadStateDir,
		configName:      "config.json",
		credentialsName: "credentials.json",
		lockName:        "credentials.lock",
		uploadsName:     "uploads",
	}, nil
}

func validateStoragePath(name, path string) error {
	if path == "" || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasSuffix(path, string(filepath.Separator)) {
		return fmt.Errorf("unsafe %s path: %q", name, path)
	}
	return nil
}

func pathContains(parent, child string) bool {
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// DefaultPaths returns paths following the XDG base directory specification.
func DefaultPaths() (Paths, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	stateHome := os.Getenv("XDG_STATE_HOME")
	if configHome == "" || stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		if stateHome == "" {
			stateHome = filepath.Join(home, ".local", "state")
		}
	}
	configDir := filepath.Join(configHome, "pku-drive-cli")
	return Paths{
		ConfigDir:       configDir,
		ConfigFile:      filepath.Join(configDir, "config.json"),
		CredentialsFile: filepath.Join(configDir, "credentials.json"),
		CredentialLock:  filepath.Join(configDir, "credentials.lock"),
		UploadStateDir:  filepath.Join(stateHome, "pku-drive-cli", "uploads"),
	}, nil
}
