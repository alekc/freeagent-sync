// Package api wraps the FreeAgent SDK with the two things this tool needs on
// top of it: a client that cannot write, and a pager that walks any family
// generically.
package api

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/alekc/freeagent"
	"golang.org/x/oauth2"
)

// requestTimeout bounds a single API call. Generous, because a large page of
// bank transactions over a slow link is normal, but finite so a cron run
// cannot hang on a half-open connection.
const requestTimeout = 2 * time.Minute

// Client is a read-only FreeAgent client plus a request counter.
//
// There is deliberately no writable constructor. The read-only guarantee is
// structural: no argument to anything in this package produces a client that
// can issue a mutating verb. The write path, when it exists, gets its own
// constructor in its own package.
type Client struct {
	fa       *freeagent.Client
	env      freeagent.Environment
	requests atomic.Int64
}

// Options configures a read-only client.
type Options struct {
	// Environment is the FreeAgent deployment to talk to.
	Environment freeagent.Environment
	// TokenSource supplies and refreshes the OAuth token.
	TokenSource oauth2.TokenSource
	// UserAgent identifies this tool to FreeAgent.
	UserAgent string
	// RequestsPerMinute and RequestsPerHour override the client-side rate
	// budget. Zero keeps the SDK's defaults, which already sit under the
	// published caps; lower them to leave room for interactive work.
	RequestsPerMinute int
	RequestsPerHour   int
}

// NewReadOnly builds a client that refuses every mutating verb.
func NewReadOnly(opts Options) (*Client, error) {
	if opts.TokenSource == nil {
		return nil, fmt.Errorf("api: NewReadOnly requires a token source")
	}
	if opts.Environment.BaseURL == "" {
		return nil, fmt.Errorf("api: NewReadOnly requires an environment")
	}

	settings := []freeagent.Option{
		freeagent.WithBaseURL(opts.Environment.BaseURL),
		freeagent.WithTokenSource(opts.TokenSource),
		freeagent.WithUserAgent(opts.UserAgent),
		// The whole point. Enforced inside the SDK's request construction, so
		// no call anywhere in this tool can bypass it.
		freeagent.WithReadOnly(),
	}
	if opts.RequestsPerMinute > 0 || opts.RequestsPerHour > 0 {
		settings = append(settings,
			freeagent.WithRateLimits(opts.RequestsPerMinute, opts.RequestsPerHour))
	}

	fa, err := freeagent.NewClient(settings...)
	if err != nil {
		return nil, fmt.Errorf("api: building the client: %w", err)
	}
	return &Client{fa: fa, env: opts.Environment}, nil
}

// Environment reports which deployment this client talks to.
func (c *Client) Environment() freeagent.Environment { return c.env }

// Requests reports how many API calls have been made, so a run can report
// what it spent of the rate budget rather than guessing.
func (c *Client) Requests() int64 { return c.requests.Load() }

// SDK exposes the underlying client for the typed services. It is read-only
// like everything else here.
func (c *Client) SDK() *freeagent.Client { return c.fa }

// Get issues one read against a path relative to the API root and returns the
// undecoded body alongside the pagination fields.
func (c *Client) Get(
	ctx context.Context, path string, opts *freeagent.ListOptions,
) ([]byte, *freeagent.Response, error) {
	query, err := opts.Values()
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	c.requests.Add(1)
	body, resp, err := c.fa.Raw(ctx, "GET", path, query, nil)
	if err != nil {
		return nil, resp, fmt.Errorf("api: GET %s: %w", path, err)
	}
	return body, resp, nil
}

// GetURL is Get for a resource URL out of a payload. The SDK restricts these
// to the client's own host, which matters because the URLs come from
// responses rather than from configuration.
func (c *Client) GetURL(
	ctx context.Context, ref freeagent.ResourceURL, opts *freeagent.ListOptions,
) ([]byte, *freeagent.Response, error) {
	query, err := opts.Values()
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	c.requests.Add(1)
	body, resp, err := c.fa.RawURL(ctx, "GET", ref, query, nil)
	if err != nil {
		return nil, resp, fmt.Errorf("api: GET %s: %w", ref, err)
	}
	return body, resp, nil
}
