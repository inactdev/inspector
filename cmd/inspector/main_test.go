package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newTestRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	runGitT(t, dir, "init", "-q")
	runGitT(t, dir, "config", "user.email", "test@example.com")
	runGitT(t, dir, "config", "user.name", "Test")

	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	runGitT(t, dir, "add", "-A")
	runGitT(t, dir, "commit", "-q", "-m", "initial", "--allow-empty")

	return dir
}

func runGitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// runCLI invokes run() the same way main() does, in-process - no need to
// build a separate binary for the tests to exercise the exact flag
// parsing and exit-code mapping main() performs.
func runCLI(t *testing.T, dir string, args ...string) (exitCode int, stdout, stderr string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer

	fullArgs := append([]string{"--repo", dir}, args...)
	exitCode = run(fullArgs, &outBuf, &errBuf)

	return exitCode, outBuf.String(), errBuf.String()
}

func TestCLI_Green(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true"}`})

	code, stdout, _ := runCLI(t, dir)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d", code, exitGreen)
	}
	if !strings.Contains(stdout, "green") {
		t.Fatalf("stdout = %q, want it to contain %q", stdout, "green")
	}
}

func TestCLI_Red(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "false"}`})

	code, stdout, _ := runCLI(t, dir)
	if code != exitRed {
		t.Fatalf("exit code = %d, want %d", code, exitRed)
	}
	if !strings.Contains(stdout, "red") {
		t.Fatalf("stdout = %q, want it to contain %q", stdout, "red")
	}
}

func TestCLI_RefusesWithoutConfig(t *testing.T) {
	dir := newTestRepo(t, nil)

	code, _, stderr := runCLI(t, dir)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr, ".inspector.json") {
		t.Fatalf("stderr = %q, want it to mention .inspector.json", stderr)
	}
}

func TestCLI_ClaimTextIsAccepted(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true"}`})

	code, _, _ := runCLI(t, dir, "the", "login", "flow", "is", "done")
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d", code, exitGreen)
	}
}
