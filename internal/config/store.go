package config

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/securefs"
)

// DefaultServer is the default PKU AnyShare server.
const DefaultServer = "https://disk.pku.edu.cn"

// Config contains non-secret local settings.
type Config struct {
	Server string `json:"server"`
}

// Credentials contains the OAuth client and token information for a server.
type Credentials struct {
	Server       string    `json:"server"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret"`
	RedirectURI  string    `json:"redirect_uri"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token,omitempty"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Store persists local configuration and credentials at Paths.
type Store struct {
	paths   validatedPaths
	initErr error
	dirs    dirOperations
}

type dirOperations interface {
	Ensure(path string) (*securefs.Dir, error)
	EnsureAt(parent *securefs.Dir, name string) (*securefs.Dir, error)
}

type secureDirOperations struct{}

func (secureDirOperations) Ensure(path string) (*securefs.Dir, error) {
	return securefs.EnsurePrivateDir(path)
}

func (secureDirOperations) EnsureAt(parent *securefs.Dir, name string) (*securefs.Dir, error) {
	return securefs.EnsurePrivateDirAt(parent, name)
}

// NewStore returns a Store using paths.
func NewStore(paths Paths) *Store {
	checked, err := validatePaths(paths)
	return &Store{paths: checked, initErr: err, dirs: secureDirOperations{}}
}

// LoadConfig returns the persisted configuration, or the default configuration
// before first login.
func (s *Store) LoadConfig() (Config, error) {
	if s.initErr != nil {
		return Config{}, s.initErr
	}
	dir, err := securefs.OpenPrivateDir(s.paths.configDir)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return Config{Server: DefaultServer}, nil
		}
		return Config{}, err
	}
	defer dir.Close()
	var config Config
	if err := securefs.ReadJSON0600At(dir, s.paths.configName, &config); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return Config{Server: DefaultServer}, nil
		}
		return Config{}, err
	}
	server, err := normalizeServer(config.Server)
	if err != nil {
		return Config{}, err
	}
	config.Server = server
	return config, nil
}

// SaveConfig validates and atomically persists config.
func (s *Store) SaveConfig(config Config) error {
	if s.initErr != nil {
		return s.initErr
	}
	server, err := normalizeServer(config.Server)
	if err != nil {
		return err
	}
	dir, err := s.ensureStorageDirs()
	if err != nil {
		return err
	}
	defer dir.Close()
	config.Server = server
	return securefs.WriteJSONAtomicAt(dir, s.paths.configName, config)
}

// LoadCredentials returns the stored credentials.
func (s *Store) LoadCredentials() (Credentials, error) {
	if s.initErr != nil {
		return Credentials{}, s.initErr
	}
	dir, err := securefs.OpenPrivateDir(s.paths.configDir)
	if err != nil {
		return Credentials{}, err
	}
	defer dir.Close()
	var credentials Credentials
	if err := securefs.ReadJSON0600At(dir, s.paths.credentialsName, &credentials); err != nil {
		return Credentials{}, err
	}
	server, err := normalizeServer(credentials.Server)
	if err != nil {
		return Credentials{}, err
	}
	credentials.Server = server
	return credentials, nil
}

// SaveCredentials validates and atomically persists credentials.
func (s *Store) SaveCredentials(credentials Credentials) error {
	if s.initErr != nil {
		return s.initErr
	}
	server, err := normalizeServer(credentials.Server)
	if err != nil {
		return err
	}
	dir, err := s.ensureStorageDirs()
	if err != nil {
		return err
	}
	defer dir.Close()
	credentials.Server = server
	return securefs.WriteJSONAtomicAt(dir, s.paths.credentialsName, credentials)
}

// DeleteCredentials securely removes the stored credentials. It is successful
// when no credential file exists.
func (s *Store) DeleteCredentials() error {
	if s.initErr != nil {
		return s.initErr
	}
	dir, err := securefs.OpenPrivateDir(s.paths.configDir)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil
		}
		return err
	}
	defer dir.Close()
	return securefs.Remove0600At(dir, s.paths.credentialsName)
}

// WithCredentialLock runs fn while holding the private credential lock.
func (s *Store) WithCredentialLock(ctx context.Context, fn func() error) error {
	if s.initErr != nil {
		return s.initErr
	}
	dir, err := s.dirs.Ensure(s.paths.configDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return securefs.WithLockAt(ctx, dir, s.paths.lockName, fn)
}

func (s *Store) ensureStorageDirs() (*securefs.Dir, error) {
	configDir, err := s.dirs.Ensure(s.paths.configDir)
	if err != nil {
		return nil, err
	}
	stateDir, err := s.dirs.Ensure(s.paths.stateToolDir)
	if err != nil {
		_ = configDir.Close()
		return nil, err
	}
	defer stateDir.Close()
	uploadDir, err := s.dirs.EnsureAt(stateDir, s.paths.uploadsName)
	if err != nil {
		_ = configDir.Close()
		return nil, err
	}
	if err := uploadDir.Close(); err != nil {
		_ = configDir.Close()
		return nil, err
	}
	return configDir, nil
}

func normalizeServer(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid server URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || !u.IsAbs() || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("invalid server URL: must be an absolute HTTPS URL without userinfo, query, or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}
