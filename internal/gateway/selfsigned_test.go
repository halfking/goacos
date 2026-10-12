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
func TestWritableDirFallsBackWhenConfiguredDirIsUnusable(t *testing.T) {
	// The container case precisely: the parent exists but is not writable, so
	// creating the child fails. Making the target itself read-only would not
	// reproduce it — MkdirAll succeeds on a directory that already exists.
	parent := filepath.Join(t.TempDir(), "root-owned")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Skipf("cannot create read-only directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	unusable := filepath.Join(parent, "data")

	got := writableDir(unusable, "acme")
	if got == unusable {
		t.Fatal("a directory that cannot be created must not be returned as usable")
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
