package inspector

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCheck_Success(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	result, err := RunCheck(dir, "echo hi", &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Output, "hi") {
		t.Fatalf("Output = %q, want it to contain %q", result.Output, "hi")
	}
	if !strings.Contains(stdout.String(), "hi") {
		t.Fatalf("stdout was not streamed: %q", stdout.String())
	}
}

func TestRunCheck_Failure(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	result, err := RunCheck(dir, "echo boom >&2; exit 7", &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", result.ExitCode)
	}
	if !strings.Contains(result.Output, "boom") {
		t.Fatalf("Output = %q, want it to contain %q", result.Output, "boom")
	}
}

func TestRunCheck_RunsFromRepoRoot(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	result, err := RunCheck(dir, "pwd -P", &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving symlinks for %s: %v", dir, err)
	}
	if strings.TrimSpace(result.Output) != wantDir {
		t.Fatalf("check did not run from %q: got %q", wantDir, result.Output)
	}
}
