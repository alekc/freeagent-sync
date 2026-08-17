// Package auth owns the OAuth dance and the token store.
//
// Tokens are keyed by account slug rather than by environment, which is the
// one difference from facli: two accounts on the same deployment must not
// share a token, because FreeAgent rotates refresh tokens and a refresh by
// one would invalidate the other.
package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/alekc/freeagent"
	"golang.org/x/oauth2"
)

// EnvRedirectURI overrides the loopback callback address.
const EnvRedirectURI = "FREEAGENT_REDIRECT_URI"

// DefaultRedirectURI is a loopback callback. It has to be registered on the
// FreeAgent application before the flow will accept it.
const DefaultRedirectURI = "http://localhost:8723/callback"

// Config is everything needed to obtain or use a token for one account.
type Config struct {
	ClientID     string
	ClientSecret string
	Environment  freeagent.Environment
	// TokenFile is the 0600 JSON store shared by every account.
	TokenFile string
	// Key is the account slug, which namespaces this account's token.
	Key string
	// RedirectURI overrides the loopback callback.
	RedirectURI string
}

func (c Config) validate() error {
	switch {
	case c.ClientID == "" || c.ClientSecret == "":
		return errors.New("auth: the OAuth client id and secret are both required")
	case c.Environment.TokenURL == "":
		return errors.New("auth: an environment is required")
	case c.TokenFile == "":
		return errors.New("auth: a token file is required")
	case c.Key == "":
		return errors.New("auth: a key is required; use the account slug")
	}
	return nil
}

// Redirect resolves the callback address: the explicit setting, then the
// environment, then the default.
func (c Config) Redirect() string {
	if c.RedirectURI != "" {
		return c.RedirectURI
	}
	if fromEnv := os.Getenv(EnvRedirectURI); fromEnv != "" {
		return fromEnv
	}
	return DefaultRedirectURI
}

// Store opens the token store for this account.
func (c Config) Store() (*freeagent.FileStore, error) {
	store, err := freeagent.NewFileStore(c.TokenFile, c.Key)
	if err != nil {
		return nil, fmt.Errorf("auth: opening the token store: %w", err)
	}
	return store, nil
}

func (c Config) oauth() *oauth2.Config {
	return c.Environment.OAuthConfig(c.ClientID, c.ClientSecret, c.Redirect())
}

// Source builds a token source that refreshes transparently and persists the
// rotated refresh token.
func Source(ctx context.Context, cfg Config) (*freeagent.TokenSource, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	store, err := cfg.Store()
	if err != nil {
		return nil, err
	}
	source, err := freeagent.NewTokenSource(ctx, cfg.oauth(), store)
	if err != nil {
		return nil, fmt.Errorf("auth: preparing credentials for %q: %w", cfg.Key, err)
	}
	return source, nil
}

// Info describes the stored credential without revealing it.
type Info struct {
	Key         string
	Environment string
	TokenPath   string
	HasToken    bool
	Expiry      time.Time
	ExpiresIn   string
	HasRefresh  bool
}

// Status reports on a stored token. It never returns the token itself: this
// output goes to terminals and, from there, into pasted bug reports.
func Status(ctx context.Context, cfg Config) (Info, error) {
	info := Info{Key: cfg.Key, Environment: cfg.Environment.Name, TokenPath: cfg.TokenFile}

	source, err := Source(ctx, cfg)
	if err != nil {
		return info, err
	}
	token, err := source.Peek()
	if err != nil {
		if errors.Is(err, freeagent.ErrNoToken) {
			return info, nil
		}
		return info, err
	}

	info.HasToken = true
	info.Expiry = token.Expiry
	info.ExpiresIn = freeagent.ExpiresIn(token, time.Now())
	info.HasRefresh = token.RefreshToken != ""
	return info, nil
}
