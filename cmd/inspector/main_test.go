package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/container"
	"github.com/inactdev/inspector/internal/inspector"
)

// requireDocker skips a test that genuinely needs a live container
// runtime, rather than failing on a machine without one.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := container.EnsureAvailable(); err != nil {
		t.Skipf("no usable container runtime, skipping: %v", err)
	}
}

func requirePublicationPlatform(t *testing.T) {
	t.Helper()
	if err := inspector.ValidatePublicationPlatform(); err != nil {
		t.Skipf("green publication is intentionally unavailable: %v", err)
	}
}

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

func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
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

type statusAPIStub struct {
	Request *statusRequest
	Remote  string
}

// stubGitHubStatusAPI points githubAPIBaseURL at a fake GitHub API and gives
// dir a local bare origin. observe runs while inspector is posting, after it
// staged C but before GitHub accepts the status, so it can prove the branch has
// not moved early.
func stubGitHubStatusAPI(t *testing.T, dir string, statusCode int, responseBody string, observe func(remote string)) *statusAPIStub {
	t.Helper()
	stub := &statusAPIStub{
		Request: &statusRequest{},
		Remote:  newBareTestRemote(t),
	}
	configureTestOrigin(t, dir, stub.Remote)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			return
		}
		stub.Request.Path = r.URL.Path
		stub.Request.Auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, stub.Request)
		if observe != nil {
			observe(stub.Remote)
		}
		w.WriteHeader(statusCode)
		if responseBody != "" {
			_, _ = w.Write([]byte(responseBody))
		}
	}))
	t.Cleanup(server.Close)

	prevBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = prevBaseURL })
	prevResolvePublicationTarget := resolvePublicationTarget
	resolvePublicationTarget = func(string) (inspector.PublicationTarget, error) {
		return inspector.PublicationTarget{PushURL: stub.Remote, Owner: "inactdev", Repo: "inspector"}, nil
	}
	t.Cleanup(func() { resolvePublicationTarget = prevResolvePublicationTarget })
	t.Setenv("GITHUB_TOKEN", "test-token")

	return stub
}

func newBareTestRemote(t *testing.T) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "origin.git")
	cmd := exec.Command("git", "init", "--bare", "-q", remote)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating bare remote: %v\n%s", err, out)
	}
	return remote
}

func configureTestOrigin(t *testing.T, dir, remote string) {
	t.Helper()
	runGitT(t, dir, "remote", "add", "origin", "https://github.com/inactdev/inspector.git")
	runGitT(t, dir, "remote", "set-url", "--push", "origin", remote)
}

func remoteRef(t *testing.T, remote, ref string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", remote, "rev-parse", "--verify", "--quiet", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
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

func TestCLI_GreenPublishesStatusBeforeBranch(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	runGitT(t, dir, "checkout", "-q", "-b", "local-checkout")
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	publicationBranch := "feature"
	stagingRef := inspector.StagingRefForCommit(commit)
	branchRef := "refs/heads/" + publicationBranch
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", func(remote string) {
		if got, exists := remoteRef(t, remote, stagingRef); !exists || got != commit {
			t.Errorf("staging ref during status post = (%q, %t), want (%q, true)", got, exists, commit)
		}
		if got, exists := remoteRef(t, remote, branchRef); exists {
			t.Errorf("pull-request branch moved before its status was recorded: %s = %s", branchRef, got)
		}
	})

	code, stdout, _ := runCLI(t, dir, "--branch", publicationBranch)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d", code, exitGreen)
	}
	if !strings.Contains(stdout, "green") {
		t.Fatalf("stdout = %q, want it to contain %q", stdout, "green")
	}
	if stub.Request.State != "success" {
		t.Fatalf("posted status state = %q, want success", stub.Request.State)
	}
	if stub.Request.Ctx != "inspector" {
		t.Fatalf("posted status context = %q, want %q", stub.Request.Ctx, "inspector")
	}
	if stub.Request.Auth != "Bearer test-token" {
		t.Fatalf("posted status Authorization = %q, want %q", stub.Request.Auth, "Bearer test-token")
	}
	if got, exists := remoteRef(t, stub.Remote, branchRef); !exists || got != commit {
		t.Fatalf("published branch = (%q, %t), want (%q, true)", got, exists, commit)
	}
	if got, exists := remoteRef(t, stub.Remote, stagingRef); exists {
		t.Fatalf("temporary staging ref still exists after publication: %s = %s", stagingRef, got)
	}
}

func TestCLI_RefusesCommitAlreadyOnPublicationBranch(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	publicationBranch := "feature"
	branchRef := "refs/heads/" + publicationBranch
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	runGitT(t, dir, "push", "-q", "origin", commit+":"+branchRef)

	code, stdout, stderr := runCLI(t, dir, "--branch", publicationBranch)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if strings.Contains(stdout, "green -") {
		t.Fatalf("stdout = %q, must not approve a commit that reached the branch before its status", stdout)
	}
	if !strings.Contains(stderr, "already points at checked commit") || !strings.Contains(stderr, "will not retroactively stamp") {
		t.Fatalf("stderr = %q, want it to explain the ordering refusal", stderr)
	}
	if stub.Request.State != "" {
		t.Fatalf("prematurely published commit posted status state %q", stub.Request.State)
	}
	if got, exists := remoteRef(t, stub.Remote, branchRef); !exists || got != commit {
		t.Fatalf("publication branch after refusal = (%q, %t), want (%q, true)", got, exists, commit)
	}
	if _, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); exists {
		t.Fatal("prematurely published commit created a staging ref")
	}
}

func TestCLI_RedDoesNotPublish(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "false", "image": "alpine"}`})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	previousValidatePublicationPlatform := validatePublicationPlatform
	validatePublicationPlatform = func() error { return errors.New("publication unsupported") }
	t.Cleanup(func() { validatePublicationPlatform = previousValidatePublicationPlatform })

	code, stdout, _ := runCLI(t, dir, "--branch", "feature")
	if code != exitRed {
		t.Fatalf("exit code = %d, want %d", code, exitRed)
	}
	if !strings.Contains(stdout, "red") || !strings.Contains(stdout, "not published by policy") {
		t.Fatalf("stdout = %q, want an explicit unpublished red verdict", stdout)
	}
	if stub.Request.State != "" {
		t.Fatalf("posted status state = %q, want no status for a red verdict", stub.Request.State)
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); exists {
		t.Fatalf("red verdict published branch %s", got)
	}
	if got, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); exists {
		t.Fatalf("red verdict published staging ref %s", got)
	}
}

func TestCLI_RefusesWithoutConfig(t *testing.T) {
	dir := newTestRepo(t, nil)
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)

	code, _, stderr := runCLI(t, dir, "--branch", "feature")
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr, ".inspector.json") {
		t.Fatalf("stderr = %q, want it to mention .inspector.json", stderr)
	}
	if stub.Request.State != "" {
		t.Fatalf("refusal posted status state %q", stub.Request.State)
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); exists {
		t.Fatalf("refusal published branch %s", got)
	}
	if got, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); exists {
		t.Fatalf("refusal published staging ref %s", got)
	}
}

func TestCLI_RequiresExplicitPublicationBranch(t *testing.T) {
	dir := newTestRepo(t, nil)

	code, _, stderr := runCLI(t, dir)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr, "--branch") {
		t.Fatalf("stderr = %q, want it to name the missing --branch", stderr)
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
	requireDocker(t)
	// A container's own init process is immune to a signal sent to it
	// from within its own PID namespace, even SIGKILL - a real Linux
	// pid-namespace behavior, not a bug - so the check command has to
	// kill a forked child rather than itself for this to reproduce; see
	// AGENTS.md.
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "sh -c 'kill -9 $$' && true", "image": "alpine"}`})

	code, _, stderr := runCLI(t, dir, "--branch", "feature")
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d (never a verdict) - stderr: %s", code, exitRefused, stderr)
	}
	if !strings.Contains(stderr, "killed") {
		t.Fatalf("stderr = %q, want it to describe the signal", stderr)
	}
}

func TestCLI_ReportWriteFailureStillExitsGreenWithLoudWarning(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	if err := os.WriteFile(filepath.Join(dir, ".inspector"), []byte("occupied"), 0o644); err != nil {
		t.Fatalf("occupying .inspector: %v", err)
	}
	stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)

	code, stdout, stderr := runCLI(t, dir, "--branch", "feature")
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
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)

	code, _, _ := runCLI(t, dir, "--branch", "feature", "the", "login", "flow", "is", "done")
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d", code, exitGreen)
	}
}

// A green check becomes a real verdict only after inspector publishes its
// recorded status and branch. These cover the ways that publication can fail:
// no token, no GitHub remote, and the API refusing the status. Each must fail
// loudly and never exit as if it had succeeded.

func TestCLI_GreenWithoutTokenFailsLoudly(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	if err := os.WriteFile(filepath.Join(dir, ".inspector"), []byte("occupied"), 0o644); err != nil {
		t.Fatalf("occupying .inspector: %v", err)
	}
	runGitT(t, dir, "remote", "add", "origin", "https://github.com/inactdev/inspector.git")
	t.Setenv("GITHUB_TOKEN", "")

	code, stdout, stderr := runCLI(t, dir, "--branch", "feature")
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d - a missing token must never exit as a success", code, exitRefused)
	}
	if code == exitGreen {
		t.Fatal("a missing token must never exit green")
	}
	if strings.Contains(stdout, "green -") {
		t.Fatalf("stdout = %q, must not print an unpublished green as a verdict", stdout)
	}
	if !strings.Contains(stderr, "GITHUB_TOKEN") {
		t.Fatalf("stderr = %q, want it to name the missing GITHUB_TOKEN requirement", stderr)
	}
	if !strings.Contains(stderr, "local green check result") {
		t.Fatalf("stderr = %q, want the report warning to describe only the local check result", stderr)
	}
	if strings.Contains(stderr, "green verdict above is real") {
		t.Fatalf("stderr = %q, must not claim a verdict before publication succeeds", stderr)
	}
	if !strings.Contains(stderr, "refused:") || !strings.Contains(stderr, "no staging push, status publication, or branch update was attempted") {
		t.Fatalf("stderr = %q, want a no-publication-attempt refusal", stderr)
	}
	if strings.Contains(stderr, "this is an incomplete publication") {
		t.Fatalf("stderr = %q, must not label a prepublication refusal as incomplete publication", stderr)
	}
}

func TestCLI_GreenWithoutGitHubRemoteFailsLoudly(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	t.Setenv("GITHUB_TOKEN", "test-token")

	code, _, stderr := runCLI(t, dir, "--branch", "feature")
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d - no origin remote must never exit as a success", code, exitRefused)
	}
	if !strings.Contains(stderr, "refused:") || !strings.Contains(stderr, "no staging push, status publication, or branch update was attempted") {
		t.Fatalf("stderr = %q, want a no-publication-attempt refusal", stderr)
	}
	if strings.Contains(stderr, "this is an incomplete publication") {
		t.Fatalf("stderr = %q, must not label a prepublication refusal as incomplete publication", stderr)
	}
}

func TestCLI_StatusAPIRefusalFailsLoudly(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	stub := stubGitHubStatusAPI(t, dir, http.StatusUnauthorized, `{"message":"Bad credentials"}`, nil)

	code, stdout, stderr := runCLI(t, dir, "--branch", "feature")
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d - an API refusal must never exit as a success", code, exitRefused)
	}
	if strings.Contains(stdout, "green -") {
		t.Fatalf("stdout = %q, must not print an unpublished green as a verdict", stdout)
	}
	if !strings.Contains(stderr, "Bad credentials") {
		t.Fatalf("stderr = %q, want GitHub's own rejection reason surfaced", stderr)
	}
	if !strings.Contains(stderr, warningBar) {
		t.Fatalf("stderr = %q, want the loud warning bar", stderr)
	}
	if strings.Contains(stderr, "stamp sent, outcome unconfirmed") {
		t.Fatalf("stderr = %q, must not call an explicit API rejection unconfirmed", stderr)
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); exists {
		t.Fatalf("status API refusal published branch %s", got)
	}
	if got, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); !exists || got != commit {
		t.Fatalf("staging ref after status refusal = (%q, %t), want (%q, true)", got, exists, commit)
	}
}

func TestPublishGreenLeavesStatusAndStagingWhenBranchMoveFails(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, nil)
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	branch := "feature"
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)

	if err := os.WriteFile(filepath.Join(dir, "remote.txt"), []byte("remote"), 0o644); err != nil {
		t.Fatalf("writing remote commit: %v", err)
	}
	runGitT(t, dir, "add", "remote.txt")
	runGitT(t, dir, "commit", "-q", "-m", "remote ahead")
	remoteCommit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	runGitT(t, dir, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	runGitT(t, dir, "reset", "-q", "--hard", commit)

	_, err := publishGreen(capturePublicationTarget(dir), branch, inspector.Result{Outcome: inspector.Green, Commit: commit})
	if err == nil {
		t.Fatal("expected non-fast-forward branch move to fail")
	}
	if stub.Request.State != "success" {
		t.Fatalf("posted status state = %q, want success before branch move", stub.Request.State)
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/"+branch); !exists || got != remoteCommit {
		t.Fatalf("remote branch after failed move = (%q, %t), want (%q, true)", got, exists, remoteCommit)
	}
	if got, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); !exists || got != commit {
		t.Fatalf("staging ref after failed move = (%q, %t), want (%q, true)", got, exists, commit)
	}
}

func TestPublishGreenReportsLostStatusResponseAsUnconfirmed(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, nil)
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	remote := newBareTestRemote(t)
	configureTestOrigin(t, dir, remote)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr != nil {
			t.Errorf("hijacking status response: %v", hijackErr)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)
	previousBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = previousBaseURL })
	previousResolvePublicationTarget := resolvePublicationTarget
	resolvePublicationTarget = func(string) (inspector.PublicationTarget, error) {
		return inspector.PublicationTarget{PushURL: remote, Owner: "inactdev", Repo: "inspector"}, nil
	}
	t.Cleanup(func() { resolvePublicationTarget = previousResolvePublicationTarget })
	t.Setenv("GITHUB_TOKEN", "test-token")

	_, err := publishGreen(capturePublicationTarget(dir), "feature", inspector.Result{Outcome: inspector.Green, Commit: commit})
	if err == nil || !strings.Contains(err.Error(), "stamp sent, outcome unconfirmed") {
		t.Fatalf("error = %v, want the distinct unconfirmed stamp state", err)
	}
	if _, exists := remoteRef(t, remote, "refs/heads/feature"); exists {
		t.Fatal("lost status response moved the named branch")
	}
	if got, exists := remoteRef(t, remote, inspector.StagingRefForCommit(commit)); !exists || got != commit {
		t.Fatalf("staging ref after lost status response = (%q, %t), want (%q, true)", got, exists, commit)
	}
}

func TestPublishGreenRefusesUnsupportedPlatformBeforePublication(t *testing.T) {
	previousValidatePublicationPlatform := validatePublicationPlatform
	validatePublicationPlatform = func() error { return errors.New("publication unsupported") }
	t.Cleanup(func() { validatePublicationPlatform = previousValidatePublicationPlatform })
	resolved := false
	previousResolvePublicationTarget := resolvePublicationTarget
	resolvePublicationTarget = func(string) (inspector.PublicationTarget, error) {
		resolved = true
		return inspector.PublicationTarget{}, nil
	}
	t.Cleanup(func() { resolvePublicationTarget = previousResolvePublicationTarget })

	_, err := publishGreen(publicationTargetSnapshot{repoRoot: t.TempDir()}, "feature", inspector.Result{Outcome: inspector.Green, Commit: "abc"})
	if err == nil || !strings.Contains(err.Error(), "publication unsupported") {
		t.Fatalf("error = %v, want unsupported-platform publication refusal", err)
	}
	if resolved {
		t.Fatal("unsupported platform resolved a publication target")
	}
}

func TestPublishGreenRefusesRemoteDefaultBranch(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, nil)
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)

	_, err := publishGreen(capturePublicationTarget(dir), "main", inspector.Result{Outcome: inspector.Green, Commit: commit})
	if err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("error = %v, want remote default branch refusal", err)
	}
	if stub.Request.State != "" {
		t.Fatalf("default branch refusal posted status state %q", stub.Request.State)
	}
	if _, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); exists {
		t.Fatal("default branch refusal published a staging ref")
	}
	if _, exists := remoteRef(t, stub.Remote, "refs/heads/main"); exists {
		t.Fatal("default branch refusal moved the default branch")
	}
}

func TestCLI_RefusesGreenCheckThatChangesTrackedCode(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		".inspector.json": `{"check": "printf changed > tracked.txt", "image": "alpine"}`,
		"tracked.txt":     "original",
	})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)

	code, stdout, stderr := runCLI(t, dir, "--branch", "feature")
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if strings.Contains(stdout, "green -") {
		t.Fatalf("stdout = %q, must not bless code changed by the check", stdout)
	}
	if !strings.Contains(stderr, "tracked.txt") {
		t.Fatalf("stderr = %q, want it to name the file changed by the check", stderr)
	}
	if stub.Request.State != "" {
		t.Fatalf("check-written change posted status state %q", stub.Request.State)
	}
	if _, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); exists {
		t.Fatal("check-written change published a staging ref")
	}
	if _, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); exists {
		t.Fatal("check-written change published the named branch")
	}
}

func TestCLI_RefusesRedCheckThatChangesTrackedCode(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		".inspector.json": `{"check": "printf changed > tracked.txt; false", "image": "alpine"}`,
		"tracked.txt":     "original",
	})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	previousValidatePublicationPlatform := validatePublicationPlatform
	validatePublicationPlatform = func() error { return errors.New("publication unsupported") }
	t.Cleanup(func() { validatePublicationPlatform = previousValidatePublicationPlatform })

	code, stdout, stderr := runCLI(t, dir, "--branch", "feature")
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if strings.Contains(stdout, "red -") {
		t.Fatalf("stdout = %q, must refuse rather than report red after the check changed code", stdout)
	}
	if !strings.Contains(stderr, "tracked.txt") {
		t.Fatalf("stderr = %q, want it to name the file changed by the check", stderr)
	}
	if stub.Request.State != "" {
		t.Fatalf("check-written change posted status state %q", stub.Request.State)
	}
	if _, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); exists {
		t.Fatal("check-written change published a staging ref")
	}
	if _, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); exists {
		t.Fatal("check-written change published the named branch")
	}
}

func TestPublishGreenRefusesRetargetedOriginBeforeRemoteSideEffects(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, nil)
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	runGitT(t, dir, "remote", "add", "origin", "https://github.com/owner/original.git")
	snapshot := capturePublicationTarget(dir)
	runGitT(t, dir, "remote", "set-url", "--push", "origin", "https://github.com/attacker/redirected.git")
	t.Setenv("GITHUB_TOKEN", "test-token")

	_, err := publishGreen(snapshot, "feature", inspector.Result{Outcome: inspector.Green, Commit: commit})
	if err == nil || !strings.Contains(err.Error(), "publication target changed during inspection") {
		t.Fatalf("error = %v, want publication-target change refusal", err)
	}
}

func TestPublishGreenRefusesChangedHeadBeforeRemoteSideEffects(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, nil)
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	if err := os.WriteFile(filepath.Join(dir, "changed.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("writing changed file: %v", err)
	}
	runGitT(t, dir, "add", "changed.txt")
	runGitT(t, dir, "commit", "-q", "-m", "changed")

	_, err := publishGreen(capturePublicationTarget(dir), "feature", inspector.Result{Outcome: inspector.Green, Commit: commit})
	if err == nil {
		t.Fatal("expected changed HEAD to stop publication")
	}
	if stub.Request.State != "" {
		t.Fatalf("changed HEAD posted status state %q", stub.Request.State)
	}
	if _, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); exists {
		t.Fatal("changed HEAD published a staging ref")
	}
	if _, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); exists {
		t.Fatal("changed HEAD published the named branch")
	}
}
