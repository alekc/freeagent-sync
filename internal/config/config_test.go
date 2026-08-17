package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDerivesTheLayoutFromDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvClientID, "id")
	t.Setenv(EnvClientSecret, "secret")

	cfg, err := Load(Flags{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"DBPath":     filepath.Join(dir, "freeagent.sqlite"),
		"BlobsDir":   filepath.Join(dir, "blobs"),
		"TmpDir":     filepath.Join(dir, "tmp"),
		"RecordsDir": filepath.Join(dir, "records"),
		"FilesDir":   filepath.Join(dir, "files"),
		"LockPath":   filepath.Join(dir, ".lock"),
	}
	got := map[string]string{
		"DBPath":     cfg.DBPath,
		"BlobsDir":   cfg.BlobsDir,
		"TmpDir":     cfg.TmpDir,
		"RecordsDir": cfg.RecordsDir,
		"FilesDir":   cfg.FilesDir,
		"LockPath":   cfg.LockPath,
	}
	for key, wantPath := range want {
		if got[key] != wantPath {
			t.Errorf("%s = %q, want %q", key, got[key], wantPath)
		}
	}
}

func TestLoadHonoursXDGDataHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDataHome, dir)
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvClientSecret, "")

	cfg, err := Load(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, appDir); cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
}

// A relative XDG_DATA_HOME would silently put the archive somewhere different
// depending on the working directory the cron job happened to start in.
func TestLoadRejectsRelativeXDGDataHome(t *testing.T) {
	t.Setenv(EnvDataHome, "relative/path")

	_, err := Load(Flags{})
	if err == nil {
		t.Fatal("a relative XDG_DATA_HOME was accepted")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error = %q, want it to say the path must be absolute", err)
	}
}

func TestLoadOverridesWin(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "custom.sqlite")
	blobs := filepath.Join(dir, "elsewhere")
	token := filepath.Join(dir, "token.json")

	cfg, err := Load(Flags{DataDir: dir, DBPath: db, BlobsDir: blobs, TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPath != db {
		t.Errorf("DBPath = %q, want the override %q", cfg.DBPath, db)
	}
	if cfg.BlobsDir != blobs {
		t.Errorf("BlobsDir = %q, want the override %q", cfg.BlobsDir, blobs)
	}
	if cfg.TokenFile != token {
		t.Errorf("TokenFile = %q, want the override %q", cfg.TokenFile, token)
	}
}

func TestLoadMakesRelativeOverridesAbsolute(t *testing.T) {
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvClientSecret, "")

	cfg, err := Load(Flags{DataDir: "relative-data"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.DataDir) {
		t.Errorf("DataDir = %q, want an absolute path", cfg.DataDir)
	}
	if !filepath.IsAbs(cfg.DBPath) {
		t.Errorf("DBPath = %q, want an absolute path", cfg.DBPath)
	}
}

// Half-configured credentials are almost always a typo in a shell profile,
// and would otherwise surface as an opaque OAuth failure much later.
func TestLoadRejectsHalfConfiguredCredentials(t *testing.T) {
	tests := []struct {
		name             string
		id, secret       string
		wantErrMentions  string
		shouldSucceedNow bool
	}{
		{name: "id without secret", id: "abc", wantErrMentions: EnvClientSecret},
		{name: "secret without id", secret: "abc", wantErrMentions: EnvClientID},
		{name: "neither", shouldSucceedNow: true},
		{name: "both", id: "abc", secret: "def", shouldSucceedNow: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvClientID, tc.id)
			t.Setenv(EnvClientSecret, tc.secret)

			_, err := Load(Flags{DataDir: t.TempDir()})
			if tc.shouldSucceedNow {
				if err != nil {
					t.Fatalf("Load returned %v, want success", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Load succeeded, want a pairing error")
			}
			if !strings.Contains(err.Error(), tc.wantErrMentions) {
				t.Errorf("error = %q, want it to name %s", err, tc.wantErrMentions)
			}
		})
	}
}

// Offline commands must work without credentials, so the check is a separate
// call rather than part of Load.
func TestRequireCredentials(t *testing.T) {
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvClientSecret, "")

	cfg, err := Load(Flags{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Load should not require credentials: %v", err)
	}

	err = cfg.RequireCredentials()
	if err == nil {
		t.Fatal("RequireCredentials succeeded with no credentials")
	}
	for _, want := range []string{EnvClientID, EnvClientSecret, registerURL} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %s", err, want)
		}
	}

	cfg.ClientID, cfg.ClientSecret = "id", "secret"
	if err := cfg.RequireCredentials(); err != nil {
		t.Errorf("RequireCredentials returned %v with both set", err)
	}
}

// facli and fasync must not share a token file by default: FreeAgent rotates
// refresh tokens, so a refresh by one would invalidate the other.
func TestTokenFileIsSeparateFromFacli(t *testing.T) {
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvClientSecret, "")

	cfg, err := Load(Flags{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.TokenFile, appDir) {
		t.Errorf("TokenFile = %q, want it under %s", cfg.TokenFile, appDir)
	}
}
