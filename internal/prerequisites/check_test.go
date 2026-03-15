package prerequisites

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- DirectoryCheck ---

func TestDirectoryCheck_PassesForExistingDir(t *testing.T) {
	dir := t.TempDir()
	c := NewDirectoryCheck("test", dir, Required)
	if err := c.Run(); err != nil {
		t.Fatalf("expected pass for existing directory, got: %v", err)
	}
}

func TestDirectoryCheck_FailsForMissingPath(t *testing.T) {
	c := NewDirectoryCheck("test", "/nonexistent/path/abc123", Required)
	if err := c.Run(); err == nil {
		t.Fatal("expected failure for missing directory")
	}
}

func TestDirectoryCheck_FailsForFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	c := NewDirectoryCheck("test", f, Required)
	if err := c.Run(); err == nil {
		t.Fatal("expected failure when path is a regular file")
	}
}

// --- SocketCheck ---

func TestSocketCheck_PassesForSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "t.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	c := NewSocketCheck("test", sockPath, Required)
	if err := c.Run(); err != nil {
		t.Fatalf("expected pass for Unix socket, got: %v", err)
	}
}

func TestSocketCheck_FailsForMissingPath(t *testing.T) {
	c := NewSocketCheck("test", "/nonexistent/path/abc123.sock", Required)
	if err := c.Run(); err == nil {
		t.Fatal("expected failure for missing socket")
	}
}

func TestSocketCheck_FailsForRegularFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	c := NewSocketCheck("test", f, Required)
	if err := c.Run(); err == nil {
		t.Fatal("expected failure when path is not a socket")
	}
}

// --- BinaryCheck ---

func TestBinaryCheck_PassesForKnownBinary(t *testing.T) {
	c := NewBinaryCheck("ls", Required)
	if err := c.Run(); err != nil {
		t.Fatalf("expected pass for 'ls', got: %v", err)
	}
}

func TestBinaryCheck_FailsForMissingBinary(t *testing.T) {
	c := NewBinaryCheck("nonexistent-binary-xyz-99999", Required)
	if err := c.Run(); err == nil {
		t.Fatal("expected failure for nonexistent binary")
	}
}

// --- RunAll ---

func TestRunAll_AllPass(t *testing.T) {
	dir := t.TempDir()
	checks := []Check{
		NewDirectoryCheck("dir", dir, Required),
		NewBinaryCheck("ls", Required),
	}
	if err := RunAll(checks); err != nil {
		t.Fatalf("expected all checks to pass, got: %v", err)
	}
}

func TestRunAll_RequiredFailureCausesError(t *testing.T) {
	checks := []Check{
		NewDirectoryCheck("missing", "/nonexistent/path", Required),
	}
	err := RunAll(checks)
	if err == nil {
		t.Fatal("expected error when required check fails")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error should mention the check name, got: %v", err)
	}
}

func TestRunAll_WarningDoesNotCauseError(t *testing.T) {
	checks := []Check{
		NewDirectoryCheck("optional-dir", "/nonexistent/path", Warning),
	}
	if err := RunAll(checks); err != nil {
		t.Fatalf("expected warning-only checks to pass RunAll, got: %v", err)
	}
}

func TestRunAll_MixedSeverity(t *testing.T) {
	dir := t.TempDir()
	checks := []Check{
		NewDirectoryCheck("good-dir", dir, Required),
		NewDirectoryCheck("bad-warning", "/nonexistent/path", Warning),
		NewBinaryCheck("ls", Required),
	}
	if err := RunAll(checks); err != nil {
		t.Fatalf("expected pass (only warning failed), got: %v", err)
	}
}

func TestRunAll_MultipleRequiredFailures(t *testing.T) {
	checks := []Check{
		NewDirectoryCheck("missing-a", "/nonexistent/a", Required),
		NewDirectoryCheck("missing-b", "/nonexistent/b", Required),
	}
	err := RunAll(checks)
	if err == nil {
		t.Fatal("expected error with multiple required failures")
	}
	if !strings.Contains(err.Error(), "missing-a") || !strings.Contains(err.Error(), "missing-b") {
		t.Fatalf("error should mention both failed checks, got: %v", err)
	}
}
