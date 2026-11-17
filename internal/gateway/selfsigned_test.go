package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

// The container runs as uid 10001 and cannot create the default relative data
// directory. The old failure mode there was ACME disabling itself silently,
// leaving the gateway on self-signed certificates forever with only one log
// line to show for it.
//
// 2026-10-24: this test was red in every root container, and it had been
// unnoticed because the gateway package was not part of the usual check list.
// Root bypasses permission bits, so "the parent exists but is not writable"
// cannot be reproduced as root — and the test was asserting that case, so it
// failed for a reason that had nothing to do with the code.
//
// Two cases now, and the second one holds in every environment:
//
//   - the container case (parent not writable) — only when not root;
//   - a parent that is a regular file — creating a child under it fails for
//     every user, root included.
//
// The point of the second case is that the property stays asserted where the
// first one cannot be. A criterion that quietly stops asserting anything is
// worse than no criterion: it reads as coverage.
func TestWritableDirFallsBackWhenConfiguredDirIsUnusable(t *testing.T) {
	// The container case precisely: the parent exists but is not writable, so
	// creating the child fails. Making the target itself read-only would not
	// reproduce it — MkdirAll succeeds on a directory that already exists.
	if os.Geteuid() != 0 {
		parent := filepath.Join(t.TempDir(), "root-owned")
		if err := os.Mkdir(parent, 0o500); err != nil {
			t.Skipf("cannot create read-only directory: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
		assertWritableDirFallsBack(t, filepath.Join(parent, "data"))
	}

	// Root-proof case: the parent path is a regular file, so creating a
	// child under it fails with ENOTDIR for every user.
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	assertWritableDirFallsBack(t, filepath.Join(file, "data"))
}

// assertWritableDirFallsBack is the property itself: a directory that cannot
// be created must not be returned as usable, and the fallback must be usable.
func assertWritableDirFallsBack(t *testing.T, unusable string) {
	t.Helper()
	got := writableDir(unusable, "acme")
	if got == unusable {
		t.Fatalf("a directory that cannot be created must not be returned as usable (%q)", unusable)
	}
	if got == "" {
		t.Fatal("expected a temp fallback, got an empty path (feature would silently disable)")
	}
	if err := os.MkdirAll(got, 0o700); err != nil {
		t.Fatalf("fallback path is not usable either: %v", err)
	}
}

func TestWritableDirKeepsUsableDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usable")
	if got := writableDir(dir, "acme"); got != dir {
		t.Errorf("a usable directory must be kept as configured, got %q", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the configured directory should have been created: %v", err)
	}
}

func TestWritableDirPassesThroughEmpty(t *testing.T) {
	if got := writableDir("", "acme"); got != "" {
		t.Errorf("an unset directory must stay unset, got %q", got)
	}
}
