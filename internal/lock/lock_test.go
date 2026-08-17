package lock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", ".lock")

	held, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if held.Path() != path {
		t.Errorf("Path = %q, want %q", held.Path(), path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the lock file was not created: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}

	// Released, so it can be taken again.
	again, err := Acquire(path)
	if err != nil {
		t.Fatalf("re-acquiring after release: %v", err)
	}
	_ = again.Release()
}

func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	held, err := Acquire(filepath.Join(t.TempDir(), ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Errorf("second Release returned %v, want nil", err)
	}
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Errorf("releasing a nil lock returned %v, want nil", err)
	}
}

func TestAcquireRejectsAnEmptyPath(t *testing.T) {
	t.Parallel()
	if _, err := Acquire(""); err == nil {
		t.Fatal("an empty path was accepted")
	}
}

// The contention case is what the lock exists for, and flock is per-process,
// so a real second process is the only honest way to test it.
func TestASecondProcessIsRefused(t *testing.T) {
	if os.Getenv("FASYNC_LOCK_CHILD") == "1" {
		lockChild()
		return
	}
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".lock")
	held, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	//nolint:gosec // the command is this test binary, re-executed by design
	cmd := exec.Command(os.Args[0], "-test.run=TestASecondProcessIsRefused")
	cmd.Env = append(os.Environ(), "FASYNC_LOCK_CHILD=1", "FASYNC_LOCK_PATH="+path)
	output, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("the second process took the lock; output: %s", output)
	}
	if code := cmd.ProcessState.ExitCode(); code != childRefused {
		t.Errorf("child exited %d, want %d; output: %s", code, childRefused, output)
	}
}

// childRefused is the exit code the helper uses for "lock already held", kept
// distinct from the generic failure code so the parent can tell them apart.
const childRefused = 9

func lockChild() {
	held, err := Acquire(os.Getenv("FASYNC_LOCK_PATH"))
	if err != nil {
		if errors.Is(err, ErrHeld) {
			os.Exit(childRefused)
		}
		os.Exit(1)
	}
	_ = held.Release()
	os.Exit(0)
}

// A lock the previous run left behind must not block the next one. flock is
// released by the kernel when the process dies, so this checks the file being
// present is not itself treated as held.
func TestAStaleLockFileDoesNotBlock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".lock")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	held, err := Acquire(path)
	if err != nil {
		t.Fatalf("a leftover lock file blocked a new run: %v", err)
	}
	_ = held.Release()
}
