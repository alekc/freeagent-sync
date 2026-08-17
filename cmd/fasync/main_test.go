package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alekc/freeagent-sync/internal/config"
)

// harness runs the CLI against a throwaway data directory and captures both
// streams, so tests assert on what a user would actually see.
type harness struct {
	t       *testing.T
	dataDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// The developer's own shell often has one of these set. Clear both so a
	// test never depends on the machine it runs on.
	t.Setenv(config.EnvClientID, "")
	t.Setenv(config.EnvClientSecret, "")
	t.Setenv("XDG_DATA_HOME", "")
	return &harness{t: t, dataDir: t.TempDir()}
}

func (h *harness) run(args ...string) (code int, stdout, stderr string) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	// Appended rather than inserted, so this also exercises the permuting
	// parser: a global flag has to work after the verb and its arguments.
	full := append(append([]string{}, args...), "--data-dir", h.dataDir)
	code = run(h.t.Context(), full, &out, &errOut)
	return code, out.String(), errOut.String()
}

// mustRun fails the test if the command did not succeed.
func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	code, stdout, stderr := h.run(args...)
	if code != exitOK {
		h.t.Fatalf("%v exited %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
	}
	return stdout
}

func TestUsageOnNoArgs(t *testing.T) {
	var out bytes.Buffer
	if code := run(t.Context(), nil, &out, &out); code != exitOK {
		t.Errorf("no args exited %d, want %d", code, exitOK)
	}
	for _, want := range []string{"init", "account", "status", "version"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage does not list %q", want)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(t.Context(), []string{"frobnicate"}, &out, &errOut)
	if code != exitConfig {
		t.Errorf("unknown command exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(errOut.String(), "frobnicate") {
		t.Errorf("stderr = %q, want it to name the command", errOut.String())
	}
}

func TestInitCreatesTheLayout(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("init")

	for _, dir := range []string{"blobs", "tmp", "records", "files"} {
		info, err := os.Stat(filepath.Join(h.dataDir, dir))
		if err != nil {
			t.Errorf("init did not create %s: %v", dir, err)
			continue
		}
		if perm := info.Mode().Perm(); perm != dirPerm {
			t.Errorf("%s mode = %o, want %o", dir, perm, dirPerm)
		}
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, "freeagent.sqlite")); err != nil {
		t.Errorf("init did not create the archive: %v", err)
	}
	if !strings.Contains(out, "freeagent.sqlite") {
		t.Errorf("init output does not name the archive:\n%s", out)
	}
}

func TestInitIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("init")
}

func TestAccountLifecycle(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	if out := h.mustRun("account", "add", "acme", "-env", "sandbox", "-name", "Acme Ltd"); !strings.Contains(out, "acme") {
		t.Errorf("add output = %q", out)
	}

	list := h.mustRun("account", "list")
	for _, want := range []string{"acme", "Acme Ltd", "sandbox"} {
		if !strings.Contains(list, want) {
			t.Errorf("list does not show %q:\n%s", want, list)
		}
	}

	h.mustRun("account", "remove", "acme")
	if out := h.mustRun("account", "list"); !strings.Contains(out, "no accounts configured") {
		t.Errorf("account survived removal:\n%s", out)
	}
}

// Flags after the positional must work: it is how people type, and the
// stdlib parser would otherwise ignore them silently.
func TestFlagsAfterPositionalAreHonoured(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("account", "add", "acme", "-env", "production")

	if out := h.mustRun("account", "list"); !strings.Contains(out, "production") {
		t.Errorf("the -env after the slug was ignored:\n%s", out)
	}
}

func TestAccountAddRejectsAnUnknownEnvironment(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	code, _, stderr := h.run("account", "add", "acme", "-env", "staging")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, "staging") {
		t.Errorf("stderr = %q, want it to name the bad environment", stderr)
	}
}

func TestAccountAddRejectsABadSlug(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	code, _, stderr := h.run("account", "add", "Bad Slug")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, "slug") {
		t.Errorf("stderr = %q, want it to explain the rule", stderr)
	}
}

func TestAccountAddNeedsExactlyOneSlug(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	for _, args := range [][]string{
		{"account", "add"},
		{"account", "add", "one", "two"},
	} {
		code, _, stderr := h.run(args...)
		if code != exitConfig {
			t.Errorf("%v exited %d, want %d", args, code, exitConfig)
		}
		if !strings.Contains(stderr, "Usage:") {
			t.Errorf("%v stderr = %q, want usage", args, stderr)
		}
	}
}

func TestStatusOnAnEmptyArchive(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	out := h.mustRun("status")
	if !strings.Contains(out, "Nothing configured yet") {
		t.Errorf("status does not prompt for an account:\n%s", out)
	}
}

// The Files-area gap is invisible from inside the archive, so status carries
// it every time rather than letting the mirror imply it is complete.
func TestStatusCarriesTheFilesAreaCaveat(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("account", "add", "acme")

	out := h.mustRun("status")
	if !strings.Contains(out, "Smart Capture") {
		t.Errorf("status does not mention the unreachable Files area:\n%s", out)
	}
	for _, want := range []string{"acme", "schema", "never"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not report %q:\n%s", want, out)
		}
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	if out := h.mustRun("version"); strings.TrimSpace(out) != Version {
		t.Errorf("version = %q, want %q", strings.TrimSpace(out), Version)
	}
}

// Half-configured credentials should stop the command before it does any
// work, and the message should name the variable that is missing.
func TestHalfConfiguredCredentialsFailEarly(t *testing.T) {
	h := newHarness(t)
	t.Setenv(config.EnvClientID, "set-but-lonely")

	code, _, stderr := h.run("status")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, config.EnvClientSecret) {
		t.Errorf("stderr = %q, want it to name %s", stderr, config.EnvClientSecret)
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{3 * 1024 * 1024 * 1024, "3.0 GB"},
	}
	for _, tc := range tests {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
