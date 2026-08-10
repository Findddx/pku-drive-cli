package anyshare

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Findddx/pku-drive-cli/internal/securefs"
)

const (
	objectSPKIPinEnvironment = "PKU_DRIVE_OBJECT_SPKI_SHA256"
	pkuControlServer         = "https://disk.pku.edu.cn"
	objectPinFileName        = "object-pin.json"
)

var errInvalidObjectPinConfig = errors.New("invalid object-store trust configuration")

type objectPinConfig struct {
	SPKISHA256 string
}

// configuredObjectSPKIPin keeps the process-local override compatible for all
// servers. The persistent compatibility pin is deliberately narrower: it is
// considered only for the exact deployed PKU control-plane origin.
func configuredObjectSPKIPin(controlServer string) (string, error) {
	if rawPin := os.Getenv(objectSPKIPinEnvironment); rawPin != "" {
		return rawPin, nil
	}
	if controlServer != pkuControlServer {
		return "", nil
	}
	configHome, err := objectPinConfigHome()
	if err != nil {
		return "", errInvalidObjectPinConfig
	}
	path := filepath.Join(configHome, "pku-drive-cli", objectPinFileName)
	var config objectPinConfig
	if err := securefs.ReadJSON0600(path, &config); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", errInvalidObjectPinConfig
		}
		config, err = readSystemObjectPinConfig(pkuSystemObjectPinLocation)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", nil
			}
			return "", errInvalidObjectPinConfig
		}
	}
	if !validObjectSPKIPin(config.SPKISHA256) {
		return "", errInvalidObjectPinConfig
	}
	return config.SPKISHA256, nil
}

func objectPinConfigHome() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errInvalidObjectPinConfig
		}
		cleanHome, ok := cleanAbsoluteConfigRoot(home)
		if !ok {
			return "", errInvalidObjectPinConfig
		}
		configHome = filepath.Join(cleanHome, ".config")
	}
	cleanConfigHome, ok := cleanAbsoluteConfigRoot(configHome)
	if !ok {
		return "", errInvalidObjectPinConfig
	}
	return cleanConfigHome, nil
}

func cleanAbsoluteConfigRoot(path string) (string, bool) {
	if path == "" || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) {
		return "", false
	}
	return filepath.Clean(path), true
}

func validObjectSPKIPin(rawPin string) bool {
	if len(rawPin) != 64 || rawPin != strings.ToLower(rawPin) {
		return false
	}
	for i := 0; i < len(rawPin); i++ {
		if !((rawPin[i] >= '0' && rawPin[i] <= '9') || (rawPin[i] >= 'a' && rawPin[i] <= 'f')) {
			return false
		}
	}
	return true
}

// UnmarshalJSON accepts exactly one string field and rejects unknown or
// duplicate fields so a security-sensitive file has one unambiguous meaning.
func (c *objectPinConfig) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return errInvalidObjectPinConfig
	}
	seen := false
	for decoder.More() {
		field, err := decoder.Token()
		name, ok := field.(string)
		if err != nil || !ok || name != "spki_sha256" || seen {
			return errInvalidObjectPinConfig
		}
		if err := decoder.Decode(&c.SPKISHA256); err != nil {
			return errInvalidObjectPinConfig
		}
		seen = true
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || !seen {
		return errInvalidObjectPinConfig
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errInvalidObjectPinConfig
	}
	return nil
}
