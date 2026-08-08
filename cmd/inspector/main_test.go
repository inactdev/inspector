package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// statusRequest is what the fake GitHub API in stubGitHubStatusAPI
// records about the one request it expects per test.
type statusRequest struct {
	Path  string
	Auth  string
	State string `json:"state"`
	Ctx   string `json:"context"`
	Desc  string `json:"description"`
}

// stubGitHubStatusAPI points githubAPIBaseURL at a fake GitHub API for
// the duration of the test, and gives dir an origin remote plus
// GITHUB_TOKEN so postCommitStatus has everything real posting needs.
// statusCode is the response the fake API returns; the recorded request
// is captured in the returned pointer regardless of what the caller's
// check outcome ends up being.
func stubGitHubStatusAPI(t *testing.T, dir string, statusCode int, responseBody string) *statusRequest {
	t.Helper()
	got := &statusRequest{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Path = r.URL.Path
		got.Auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, got)
		w.WriteHeader(statusCode)
		if responseBody != "" {
			w.Write([]byte(responseBody))
		}
	}))
	t.Cleanup(server.Close)

	prevBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = prevBaseURL })

	runGitT(t, dir, "remote", "add", "origin", "https://github.com/inactdev/inspector.git")
	t.Setenv("GITHUB_TOKEN", "test-token")

	return got
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
	got := stubGitHubStatusAPI(t, dir, http.StatusCreated, "")

	code, stdout, _ := runCLI(t, dir)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d", code, exitGreen)
	}
	if !strings.Contains(stdout, "green") {
		t.Fatalf("stdout = %q, want it to contain %q", stdout, "green")
	}
	if got.State != "success" {
		t.Fatalf("posted status state = %q, want success", got.State)
	}
	if got.Ctx != "inspector" {
		t.Fatalf("posted status context = %q, want %q", got.Ctx, "inspector")
	}
	if got.Auth != "Bearer test-token" {
		t.Fatalf("posted status Authorization = %q, want %q", got.Auth, "Bearer test-token")
	}
}

func TestCLI_Red(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "false"}`})
	got := stubGitHubStatusAPI(t, dir, http.StatusCreated, "")

	code, stdout, _ := runCLI(t, dir)
	if code != exitRed {
		t.Fatalf("exit code = %d, want %d", code, exitRed)
	}
	if !strings.Contains(stdout, "red") {
		t.Fatalf("stdout = %q, want it to contain %q", stdout, "red")
	}
	if got.State != "failure" {
		t.Fatalf("posted status state = %q, want failure", got.State)
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

func TestCLI_HelpExitsWithUsageCode(t *testing.T) {
	dir := newTestRepo(t, nil)

	code, _, stderr := runCLI(t, dir, "--help")
	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d (not a verdict code)", code, exitUsage)
	}
	if code == exitGreen || code == exitRed || code == exitRefused {
		t.Fatalf("--help exit code %d collides with a verdict code", code)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Fatalf("stderr = %q, want usage text", stderr)
	}
}

func TestCLI_UnknownFlagExitsWithUsageCode(t *testing.T) {
	dir := newTestRepo(t, nil)

	code, _, _ := runCLI(t, dir, "--not-a-real-flag")
	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d (not a verdict code)", code, exitUsage)
	}
}

func TestCLI_SignalKilledCheckExitsRefused(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "kill -9 $$"}`})

	code, _, stderr := runCLI(t, dir)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d (never a verdict) - stderr: %s", code, exitRefused, stderr)
	}
	if !strings.Contains(stderr, "killed") {
		t.Fatalf("stderr = %q, want it to describe the signal", stderr)
	}
}

func TestCLI_ReportWriteFailureStillExitsGreenWithLoudWarning(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true"}`})
	if err := os.WriteFile(filepath.Join(dir, ".inspector"), []byte("occupied"), 0o644); err != nil {
		t.Fatalf("occupying .inspector: %v", err)
	}
	stubGitHubStatusAPI(t, dir, http.StatusCreated, "")

	code, stdout, stderr := runCLI(t, dir)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d - a reached verdict must survive a report-write failure", code, exitGreen)
	}
	if !strings.Contains(stdout, "green") {
		t.Fatalf("stdout = %q, want it to still report green", stdout)
	}
	if !strings.Contains(stderr, warningBar) {
		t.Fatalf("stderr = %q, want the loud warning bar", stderr)
	}
	if !strings.Contains(stderr, ".inspector") {
		t.Fatalf("stderr = %q, want the warning to say where it failed", stderr)
	}
}

func TestCLI_ClaimTextIsAccepted(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true"}`})
	stubGitHubStatusAPI(t, dir, http.StatusCreated, "")

	code, _, _ := runCLI(t, dir, "the", "login", "flow", "is", "done")
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d", code, exitGreen)
	}
}

// A green or red check is a real local result, but SPEC.md section 7
// makes the posted commit status - not that local result - the thing
// the gate reads. These cover the ways posting it can fail: no token
// at all, no GitHub remote to post to, and the API itself refusing the
// request. Each must fail loudly and never exit as if it had succeeded.

func TestCLI_GreenWithoutTokenFailsLoudly(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true"}`})
	runGitT(t, dir, "remote", "add", "origin", "https://github.com/inactdev/inspector.git")
	t.Setenv("GITHUB_TOKEN", "")

	code, stdout, stderr := runCLI(t, dir)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d - a missing token must never exit as a success", code, exitRefused)
	}
	if code == exitGreen {
		t.Fatal("a missing token must never exit green")
	}
	if !strings.Contains(stdout, "green") {
		t.Fatalf("stdout = %q, want the real local verdict still printed", stdout)
	}
	if !strings.Contains(stderr, "GITHUB_TOKEN") {
		t.Fatalf("stderr = %q, want it to name the missing GITHUB_TOKEN requirement", stderr)
	}
}

func TestCLI_GreenWithoutGitHubRemoteFailsLoudly(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true"}`})
	t.Setenv("GITHUB_TOKEN", "test-token")

	code, _, stderr := runCLI(t, dir)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d - no origin remote must never exit as a success", code, exitRefused)
	}
	if !strings.Contains(stderr, "could not post a commit status") {
		t.Fatalf("stderr = %q, want it to say the status could not be posted", stderr)
	}
}

func TestCLI_StatusAPIRefusalFailsLoudly(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true"}`})
	stubGitHubStatusAPI(t, dir, http.StatusUnauthorized, `{"message":"Bad credentials"}`)

	code, stdout, stderr := runCLI(t, dir)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d - an API refusal must never exit as a success", code, exitRefused)
	}
	if !strings.Contains(stdout, "green") {
		t.Fatalf("stdout = %q, want the real local verdict still printed", stdout)
	}
	if !strings.Contains(stderr, "Bad credentials") {
		t.Fatalf("stderr = %q, want GitHub's own rejection reason surfaced", stderr)
	}
	if !strings.Contains(stderr, warningBar) {
		t.Fatalf("stderr = %q, want the loud warning bar", stderr)
	}
}
