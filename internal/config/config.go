// Package config resolves paths and credentials once, at startup, so the
// rest of the tool can use the values without re-deriving or defaulting them.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Environment variables read here. Credentials are never stored in the
// archive or in a config file.
const (
	EnvClientID     = "FREEAGENT_CLIENT_ID"
	EnvClientSecret = "FREEAGENT_CLIENT_SECRET"
	EnvDataHome     = "XDG_DATA_HOME"
)

// appDir is the directory name used under the data home and the config home.
const appDir = "freeagent-sync"

// registerURL is where a user obtains the credentials below. A literal URL in
// an error message is documentation, not a runtime fallback.
const registerURL = "https://dev.freeagent.com/apps"

// Flags carries the path overrides a command accepted, before defaults are
// applied. An empty field means "not set".
type Flags struct {
	DataDir   string
	DBPath    string
	BlobsDir  string
	TokenFile string
}

// Config is the resolved layout. Every path is absolute.
type Config struct {
	DataDir    string
	DBPath     string
	BlobsDir   string
	TmpDir     string
	RecordsDir string
	FilesDir   string
	LockPath   string
	TokenFile  string

	ClientID     string
	ClientSecret string
}

// Load resolves the layout. It deliberately does not check credentials:
// several commands are entirely offline, so requiring a token to run
// `files rebuild` would be wrong. Commands that call the API ask for them
// explicitly through RequireCredentials.
func Load(flags Flags) (*Config, error) {
	dataDir, err := resolveDataDir(flags.DataDir)
	if err != nil {
		return nil, err
	}
	tokenFile, err := resolveTokenFile(flags.TokenFile)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		DataDir:      dataDir,
		DBPath:       or(flags.DBPath, filepath.Join(dataDir, "freeagent.sqlite")),
		BlobsDir:     or(flags.BlobsDir, filepath.Join(dataDir, "blobs")),
		TmpDir:       filepath.Join(dataDir, "tmp"),
		RecordsDir:   filepath.Join(dataDir, "records"),
		FilesDir:     filepath.Join(dataDir, "files"),
		LockPath:     filepath.Join(dataDir, ".lock"),
		TokenFile:    tokenFile,
		ClientID:     os.Getenv(EnvClientID),
		ClientSecret: os.Getenv(EnvClientSecret),
	}
	if err := cfg.absolutise(); err != nil {
		return nil, err
	}
	return cfg, cfg.checkCredentialPairing()
}

// RequireCredentials reports whether the OAuth client is configured, for the
// commands that talk to the API.
func (c *Config) RequireCredentials() error {
	if c.ClientID == "" || c.ClientSecret == "" {
		return fmt.Errorf(
			"config: set %s and %s; register an application at %s",
			EnvClientID, EnvClientSecret, registerURL)
	}
	return nil
}

// checkCredentialPairing catches the half-configured case at startup. One
// variable set and the other missing is almost always a typo in a shell
// profile, and it would otherwise surface as an opaque OAuth failure.
func (c *Config) checkCredentialPairing() error {
	switch {
	case c.ClientID != "" && c.ClientSecret == "":
		return fmt.Errorf("config: %s is set but %s is not", EnvClientID, EnvClientSecret)
	case c.ClientID == "" && c.ClientSecret != "":
		return fmt.Errorf("config: %s is set but %s is not", EnvClientSecret, EnvClientID)
	}
	return nil
}

func (c *Config) absolutise() error {
	paths := []*string{
		&c.DataDir, &c.DBPath, &c.BlobsDir, &c.TmpDir,
		&c.RecordsDir, &c.FilesDir, &c.LockPath, &c.TokenFile,
	}
	for _, p := range paths {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return fmt.Errorf("config: resolving %q: %w", *p, err)
		}
		*p = abs
	}
	return nil
}

func resolveDataDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if xdg := os.Getenv(EnvDataHome); xdg != "" {
		if !filepath.IsAbs(xdg) {
			return "", fmt.Errorf("config: %s must be an absolute path, got %q", EnvDataHome, xdg)
		}
		return filepath.Join(xdg, appDir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"config: cannot locate a home directory, pass --data-dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", appDir), nil
}

// resolveTokenFile keeps this tool's tokens separate from facli's by default.
// FreeAgent rotates refresh tokens, so two tools sharing one file can
// invalidate each other's credentials; pointing --token-file at facli's is
// supported but opts into that.
func resolveTokenFile(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf(
			"config: cannot locate a config directory, pass --token-file: %w", err)
	}
	return filepath.Join(dir, appDir, "token.json"), nil
}

func or(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// ErrNoAccount is returned when a command needs an account and none was
// selected or stored.
var ErrNoAccount = errors.New("config: no account selected, use --account or run: fasync account add")
