package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CallbackTimeout bounds how long login waits for the browser round trip.
const CallbackTimeout = 5 * time.Minute

// LoginOptions configures the authorisation flow.
type LoginOptions struct {
	// Manual reads the code from stdin instead of listening for the callback,
	// for a headless machine or a redirect that is not loopback.
	Manual bool
	// In is where a manual code is read from.
	In io.Reader
	// Out is where the authorisation URL and progress are written.
	Out io.Writer
}

// Result reports what login obtained.
type Result struct {
	TokenPath  string
	ExpiresIn  string
	HasRefresh bool
}

// Login runs the authorisation code flow and stores the resulting token.
func Login(ctx context.Context, cfg Config, opts LoginOptions) (Result, error) {
	var result Result
	source, err := Source(ctx, cfg)
	if err != nil {
		return result, err
	}
	state, err := randomState()
	if err != nil {
		return result, err
	}

	redirect := cfg.Redirect()
	var (
		listener net.Listener
		codes    chan callbackResult
		serveErr chan error
	)
	if !opts.Manual {
		listener, codes, serveErr, err = startCallbackServer(ctx, redirect, state)
		if err != nil {
			return result, fmt.Errorf("%w\nuse -manual to paste the code instead", err)
		}
		defer func() { _ = listener.Close() }()
	}

	_, _ = fmt.Fprintf(opts.Out,
		"Account:      %s\nEnvironment:  %s\nRedirect URI: %s\n\n"+
			"Open this URL and approve the application:\n\n%s\n\n",
		cfg.Key, cfg.Environment.Name, redirect, source.AuthCodeURL(state))

	var code string
	if opts.Manual {
		code, err = readCode(opts)
	} else {
		_, _ = fmt.Fprintf(opts.Out, "Waiting up to %s for the callback on %s\n",
			CallbackTimeout, redirect)
		code, err = waitForCallback(ctx, codes, serveErr)
	}
	if err != nil {
		return result, err
	}

	token, err := source.Exchange(ctx, code)
	if err != nil {
		return result, fmt.Errorf("auth: exchanging the authorisation code: %w", err)
	}

	store, err := cfg.Store()
	if err != nil {
		return result, err
	}
	result.TokenPath = store.Path()
	result.ExpiresIn = expiresIn(token.Expiry)
	result.HasRefresh = token.RefreshToken != ""

	// A refresh token is what makes an unattended cron run possible. Without
	// one the credential dies at the first expiry, so say so now rather than
	// letting it fail at 3am.
	if !result.HasRefresh {
		return result, errors.New(
			"auth: no refresh token was issued, so this credential cannot renew itself")
	}
	return result, nil
}

type callbackResult struct {
	code string
	err  error
}

// startCallbackServer listens on the loopback address in the redirect URI and
// resolves once the browser arrives with a matching state.
func startCallbackServer(
	ctx context.Context, redirect, state string,
) (net.Listener, chan callbackResult, chan error, error) {
	u, err := url.Parse(redirect)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("auth: invalid redirect URI %q: %w", redirect, err)
	}
	if u.Scheme != "http" || !isLoopback(u.Hostname()) {
		return nil, nil, nil, fmt.Errorf(
			"auth: redirect URI %q is not a loopback http address, so nothing can listen for it",
			redirect)
	}

	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", u.Host)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("auth: listening on %s: %w", u.Host, err)
	}

	results := make(chan callbackResult, 1)
	errs := make(chan error, 1)
	path := u.Path
	if path == "" {
		path = "/"
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		results <- handleCallback(w, r, state)
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	return listener, results, errs, nil
}

func handleCallback(w http.ResponseWriter, r *http.Request, state string) callbackResult {
	query := r.URL.Query()
	switch {
	case query.Get("error") != "":
		err := fmt.Errorf("auth: authorisation denied: %s %s",
			query.Get("error"), query.Get("error_description"))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return callbackResult{err: err}

	case query.Get("state") != state:
		// A mismatch means this is not the response this process started, so
		// the code cannot be trusted.
		err := errors.New("auth: state mismatch in the OAuth callback")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return callbackResult{err: err}

	case query.Get("code") == "":
		err := errors.New("auth: the callback carried no authorisation code")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return callbackResult{err: err}

	default:
		_, _ = io.WriteString(w,
			"Authorisation received. You can close this tab and return to the terminal.")
		return callbackResult{code: query.Get("code")}
	}
}

func waitForCallback(
	ctx context.Context, results chan callbackResult, errs chan error,
) (string, error) {
	timer := time.NewTimer(CallbackTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case err := <-errs:
		return "", fmt.Errorf("auth: callback server: %w", err)
	case res := <-results:
		return res.code, res.err
	case <-timer.C:
		return "", fmt.Errorf("auth: no callback within %s", CallbackTimeout)
	}
}

func readCode(opts LoginOptions) (string, error) {
	_, _ = fmt.Fprint(opts.Out, "Paste the code parameter from the redirect URL: ")
	line, err := bufio.NewReader(opts.In).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("auth: reading the authorisation code: %w", err)
	}
	code := strings.TrimSpace(line)
	if code == "" {
		return "", errors.New("auth: no authorisation code given")
	}
	return code, nil
}

func randomState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generating OAuth state: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func expiresIn(expiry time.Time) string {
	if expiry.IsZero() {
		return "unknown"
	}
	remaining := time.Until(expiry).Round(time.Second)
	if remaining <= 0 {
		return "expired"
	}
	return remaining.String()
}
