package anyshare

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemObjectPinConfigEnablesPinnedObjectTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	digest := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	location := installTestSystemObjectPin(t, hex.EncodeToString(digest[:]))
	withTestSystemObjectPinLocation(t, location)
	t.Setenv(objectSPKIPinEnvironment, "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "missing-user-config"))

	client := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	if err := configurePinnedObjectTransport(client, "127.0.0.1", pkuControlServer); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestConfiguredObjectPinFallsBackToRootManagedSystemConfig(t *testing.T) {
	location := installTestSystemObjectPin(t, strings.Repeat("a", 64))
	if _, err := readSystemObjectPinConfig(location); err != nil {
		t.Fatalf("read system config: %v", err)
	}
	withTestSystemObjectPinLocation(t, location)
	t.Setenv(objectSPKIPinEnvironment, "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "missing-user-config"))

	got, err := configuredObjectSPKIPin(pkuControlServer)
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.Repeat("a", 64) {
		t.Fatalf("pin = %q", got)
	}
}

func TestConfiguredObjectPinUserConfigPrecedesSystemConfig(t *testing.T) {
	location := installTestSystemObjectPin(t, strings.Repeat("b", 64))
	withTestSystemObjectPinLocation(t, location)
	t.Setenv(objectSPKIPinEnvironment, "")
	configHome := filepath.Join(t.TempDir(), "user-config")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeInternalObjectPin(t, configHome, strings.Repeat("a", 64), 0o600)

	got, err := configuredObjectSPKIPin(pkuControlServer)
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.Repeat("a", 64) {
		t.Fatalf("pin = %q", got)
	}
}

func TestConfiguredObjectPinUnsafeUserConfigDoesNotFallThroughToSystem(t *testing.T) {
	location := installTestSystemObjectPin(t, strings.Repeat("b", 64))
	withTestSystemObjectPinLocation(t, location)
	t.Setenv(objectSPKIPinEnvironment, "")
	configHome := filepath.Join(t.TempDir(), "user-config")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeInternalObjectPin(t, configHome, strings.Repeat("a", 64), 0o644)

	if _, err := configuredObjectSPKIPin(pkuControlServer); !errors.Is(err, errInvalidObjectPinConfig) {
		t.Fatalf("error = %v, want fail-closed user configuration", err)
	}
}

func TestConfiguredObjectPinRejectsUnsafeSystemConfig(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T, systemObjectPinLocation)
	}{
		{name: "wrong owner", build: func(_ *testing.T, location systemObjectPinLocation) {
			pkuSystemObjectPinLocation.uid = location.uid + 1
		}},
		{name: "wrong group", build: func(_ *testing.T, location systemObjectPinLocation) {
			pkuSystemObjectPinLocation.gid = location.gid + 1
		}},
		{name: "wide directory", build: func(t *testing.T, location systemObjectPinLocation) {
			if err := os.Chmod(location.fullDirectory(), 0o775); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wide file", build: func(t *testing.T, location systemObjectPinLocation) {
			if err := os.Chmod(filepath.Join(location.fullDirectory(), objectPinFileName), 0o666); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink file", build: func(t *testing.T, location systemObjectPinLocation) {
			path := filepath.Join(location.fullDirectory(), objectPinFileName)
			target := filepath.Join(location.fullDirectory(), "target.json")
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "malformed JSON", build: func(t *testing.T, location systemObjectPinLocation) {
			path := filepath.Join(location.fullDirectory(), objectPinFileName)
			if err := os.WriteFile(path, []byte(`{"spki_sha256":"system-pin-marker"`), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			location := installTestSystemObjectPin(t, strings.Repeat("a", 64))
			withTestSystemObjectPinLocation(t, location)
			tt.build(t, location)
			t.Setenv(objectSPKIPinEnvironment, "")
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "missing-user-config"))

			_, err := configuredObjectSPKIPin(pkuControlServer)
			if !errors.Is(err, errInvalidObjectPinConfig) {
				t.Fatalf("error = %v, want invalid trust configuration", err)
			}
			if err != nil && strings.Contains(err.Error(), "system-pin-marker") {
				t.Fatalf("error leaked file contents: %v", err)
			}
		})
	}
}

func installTestSystemObjectPin(t *testing.T, pin string) systemObjectPinLocation {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "system-config")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, objectPinFileName)
	if err := os.WriteFile(path, []byte(`{"spki_sha256":"`+pin+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return systemObjectPinLocation{
		root:      root,
		directory: "system-config",
		uid:       uint32(os.Geteuid()),
		gid:       uint32(os.Getegid()),
	}
}

func withTestSystemObjectPinLocation(t *testing.T, location systemObjectPinLocation) {
	t.Helper()
	previous := pkuSystemObjectPinLocation
	pkuSystemObjectPinLocation = location
	t.Cleanup(func() { pkuSystemObjectPinLocation = previous })
}

func writeInternalObjectPin(t *testing.T, configHome, pin string, mode os.FileMode) {
	t.Helper()
	directory := filepath.Join(configHome, "pku-drive-cli")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, objectPinFileName)
	if err := os.WriteFile(path, []byte(`{"spki_sha256":"`+pin+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
