package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// readRepoFile reads a file from the repository under inspection, for
// the tests that have to prove a check command left it exactly as the
// commit has it.
func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
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

// pullRequestRecord is one pull request the fake GitHub API has created.
type pullRequestRecord struct {
	Head, Base, Title, Body, URL string
}

// fakePullRequests is the pull-request half of the fake GitHub API: a
// list endpoint inspector checks before ever creating one, and a create
// endpoint that records what it was asked to open. Tests read Created and
// the request counts after runCLI returns, once the HTTP round trip that
// produced them has already completed.
type fakePullRequests struct {
	Created      []pullRequestRecord
	ListRequests int
	nextNumber   int
}

func (p *fakePullRequests) handleList(w http.ResponseWriter, r *http.Request) {
	p.ListRequests++
	_, branch, _ := strings.Cut(r.URL.Query().Get("head"), ":")
	base := r.URL.Query().Get("base")
	matches := []map[string]string{}
	for _, rec := range p.Created {
		if rec.Head == branch && rec.Base == base {
			matches = append(matches, map[string]string{"html_url": rec.URL})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(matches)
	_, _ = w.Write(data)
}

func (p *fakePullRequests) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	p.nextNumber++
	url := fmt.Sprintf("https://github.com/inactdev/inspector/pull/%d", p.nextNumber)
	p.Created = append(p.Created, pullRequestRecord{Head: body.Head, Base: body.Base, Title: body.Title, Body: body.Body, URL: url})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	data, _ := json.Marshal(map[string]string{"html_url": url})
	_, _ = w.Write(data)
}

type statusAPIStub struct {
	// Request is the FIRST status post the stub saw, which for `inspector
	// run` is always the "inspector" one - the publication order guarantees
	// it. Tests that only care about that one keep reading it.
	Request *statusRequest
	// Requests is every status post in the order they arrived, so a test can
	// assert on the separate "inspector-tests" post (issue #44) as well.
	Requests     []statusRequest
	Remote       string
	PullRequests *fakePullRequests
}

// statusFor returns the status posted under the given context, and whether one
// was posted at all - the distinction the inspector-tests status depends on,
// since "no test file changed" means no post rather than a passing post.
func (s *statusAPIStub) statusFor(context string) (statusRequest, bool) {
	for _, req := range s.Requests {
		if req.Ctx == context {
			return req, true
		}
	}
	return statusRequest{}, false
}

// stubGitHubStatusAPI points githubAPIBaseURL at a fake GitHub API and gives
// dir a local bare origin. observe runs while inspector is posting, after it
// staged C but before GitHub accepts the status, so it can prove the branch has
// not moved early.
func stubGitHubStatusAPI(t *testing.T, dir string, statusCode int, responseBody string, observe func(remote string)) *statusAPIStub {
	t.Helper()
	stub := &statusAPIStub{
		Request:      &statusRequest{},
		Remote:       newBareTestRemote(t),
		PullRequests: &fakePullRequests{},
	}
	configureTestOrigin(t, dir, stub.Remote)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			stub.PullRequests.handleList(w, r)
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			stub.PullRequests.handleCreate(w, r)
			return
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			return
		}
		posted := statusRequest{Path: r.URL.Path, Auth: r.Header.Get("Authorization")}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &posted)
		stub.Requests = append(stub.Requests, posted)
		if len(stub.Requests) == 1 {
			*stub.Request = posted
		}
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
	if !strings.Contains(stderr, "already points at checked commit") || !strings.Contains(stderr, "will not retroactively stamp") || !strings.Contains(stderr, "new local commit") || !strings.Contains(stderr, "before anything publishes it") {
		t.Fatalf("stderr = %q, want it to explain the ordering refusal and recovery", stderr)
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

	_, _, err := publishGreen(capturePublicationTarget(dir), publicationBranchTarget{Branch: branch}, inspector.Result{Outcome: inspector.Green, Commit: commit}, testJudgmentOptions{})
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

// rewriteRemoteBranch leaves dir's HEAD holding a commit the remote branch
// does not contain - what a rebase produces - and returns the commit the
// remote branch points at and the rewritten local one.
func rewriteRemoteBranch(t *testing.T, dir, remote, branch string) (remoteCommit, rewritten string) {
	t.Helper()
	remoteCommit = strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	runGitT(t, dir, "push", "-q", remote, remoteCommit+":refs/heads/"+branch)
	runGitT(t, dir, "commit", "-q", "--amend", "--allow-empty", "-m", "rebased")
	rewritten = strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	if rewritten == remoteCommit {
		t.Fatal("test setup: amending produced the same commit, so nothing was rewritten")
	}
	return remoteCommit, rewritten
}

func TestCLI_GreenWithExpectedTipPublishesRebasedBranch(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	remoteCommit, rewritten := rewriteRemoteBranch(t, dir, stub.Remote, "feature")

	code, stdout, stderr := runCLI(t, dir, "--branch", "feature", "--expected-tip", remoteCommit)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, exitGreen, stderr)
	}
	if !strings.Contains(stdout, "green - "+rewritten) {
		t.Fatalf("stdout = %q, want green on the rebased commit %s", stdout, rewritten)
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); !exists || got != rewritten {
		t.Fatalf("published branch = (%q, %t), want (%q, true)", got, exists, rewritten)
	}
	if status, posted := stub.statusFor("inspector"); !posted || status.State != "success" {
		t.Fatalf("inspector status = (%+v, %t), want a posted success", status, posted)
	}
	if _, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(rewritten)); exists {
		t.Fatal("temporary staging ref still exists after publication")
	}
}

// TestCLI_GreenWithoutExpectedTipLeavesARebasedBranchUnpublished pins the
// behavior the flag exists to opt out of: with no lease named, the branch
// update is fast-forward only, so a rebased commit gets its green stamp and
// then cannot move the branch - an incomplete publication, never a green exit.
func TestCLI_GreenWithoutExpectedTipLeavesARebasedBranchUnpublished(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	remoteCommit, _ := rewriteRemoteBranch(t, dir, stub.Remote, "feature")

	code, _, stderr := runCLI(t, dir, "--branch", "feature")
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr, "incomplete publication") {
		t.Fatalf("stderr = %q, want it reported as an incomplete publication", stderr)
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); !exists || got != remoteCommit {
		t.Fatalf("remote branch = (%q, %t), want it left at %s", got, exists, remoteCommit)
	}
}

func TestCLI_RefusesAnExpectedTipThatIsNotAFullSHA(t *testing.T) {
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})

	code, stdout, stderr := runCLI(t, dir, "--branch", "feature", "--expected-tip", "HEAD~1")
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr, "--expected-tip") || !strings.Contains(stderr, "complete lowercase hexadecimal commit SHA") {
		t.Fatalf("stderr = %q, want it to name the flag and what it requires", stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing run for an unusable lease", stdout)
	}
}

// TestPublishGreenPublishesARebasedBranchUnderItsLease is the publication
// sequence itself, without the container a full CLI run needs: a commit whose
// history the remote branch does not contain still gets staged, stamped, and
// then moved onto that branch, in that order.
func TestPublishGreenPublishesARebasedBranchUnderItsLease(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, nil)
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	remoteCommit, rewritten := rewriteRemoteBranch(t, dir, stub.Remote, "feature")

	_, _, err := publishGreen(
		capturePublicationTarget(dir),
		publicationBranchTarget{Branch: "feature", ExpectedTip: remoteCommit},
		inspector.Result{Outcome: inspector.Green, Commit: rewritten},
		testJudgmentOptions{},
	)
	if err != nil {
		t.Fatalf("publishGreen on a rebased branch: %v", err)
	}
	if stub.Request.State != "success" {
		t.Fatalf("posted status state = %q, want success", stub.Request.State)
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); !exists || got != rewritten {
		t.Fatalf("published branch = (%q, %t), want (%q, true)", got, exists, rewritten)
	}
	if _, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(rewritten)); exists {
		t.Fatal("temporary staging ref still exists after publication")
	}
}

func TestPublishGreenRefusesAnExpectedTipTheRemoteLeft(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, nil)
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	remoteCommit, rewritten := rewriteRemoteBranch(t, dir, stub.Remote, "feature")
	staleTip := strings.Repeat("a", len(remoteCommit))

	_, _, err := publishGreen(
		capturePublicationTarget(dir),
		publicationBranchTarget{Branch: "feature", ExpectedTip: staleTip},
		inspector.Result{Outcome: inspector.Green, Commit: rewritten},
		testJudgmentOptions{},
	)
	if err == nil {
		t.Fatal("expected a refusal when the remote is not at the expected tip")
	}
	var publicationErr *publicationError
	if !errors.As(err, &publicationErr) || publicationErr.attempted {
		t.Fatalf("error = %v, want a refusal that attempted no remote write", err)
	}
	if !strings.Contains(err.Error(), remoteCommit) || !strings.Contains(err.Error(), staleTip) {
		t.Fatalf("error = %q, want it to name both the remote commit and the expected tip", err.Error())
	}
	if stub.Request.State != "" {
		t.Fatalf("expected-tip mismatch posted status state %q", stub.Request.State)
	}
	if _, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(rewritten)); exists {
		t.Fatal("expected-tip mismatch published a staging ref")
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); !exists || got != remoteCommit {
		t.Fatalf("remote branch = (%q, %t), want it left at %s", got, exists, remoteCommit)
	}
}

// TestPublishGreenReportsALeaseLostDuringPublication closes the gap the
// pre-flight read cannot: the remote branch moves after inspector read it and
// before the branch push runs. The lease on the push is what catches that, and
// because the remote answered, the report says the branch did not move rather
// than that the outcome is unknown. The status post is the hook that lets the
// test land its change in exactly that window.
func TestPublishGreenReportsALeaseLostDuringPublication(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, nil)
	var remotePath, moved string
	// This runs on the stub server's own goroutine, so it reports with
	// t.Errorf rather than the t.Fatalf runGitT uses.
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", func(remote string) {
		cmd := exec.Command("git", "--git-dir", remotePath, "update-ref", "refs/heads/feature", moved)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("moving the remote branch mid-publication: %v\n%s", err, out)
		}
	})
	remotePath = stub.Remote

	// A commit someone else lands on the branch, parked in the remote's
	// object store so the window below only has to move the ref.
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other work"), 0o644); err != nil {
		t.Fatalf("writing the other commit: %v", err)
	}
	runGitT(t, dir, "add", "other.txt")
	runGitT(t, dir, "commit", "-q", "-m", "someone else")
	moved = strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	runGitT(t, dir, "push", "-q", stub.Remote, moved+":refs/heads/other-work")
	runGitT(t, dir, "reset", "-q", "--hard", "HEAD~1")
	remoteCommit, rewritten := rewriteRemoteBranch(t, dir, stub.Remote, "feature")

	_, _, err := publishGreen(
		capturePublicationTarget(dir),
		publicationBranchTarget{Branch: "feature", ExpectedTip: remoteCommit},
		inspector.Result{Outcome: inspector.Green, Commit: rewritten},
		testJudgmentOptions{},
	)
	if err == nil {
		t.Fatal("expected the lease to stop a branch that moved mid-publication")
	}
	var publicationErr *publicationError
	if !errors.As(err, &publicationErr) || !publicationErr.attempted {
		t.Fatalf("error = %v, want an incomplete publication, not a refusal", err)
	}
	if !strings.Contains(err.Error(), "was not moved") || strings.Contains(err.Error(), "is unknown") {
		t.Fatalf("error = %q, want it to state the branch did not move rather than that the outcome is unknown", err.Error())
	}
	if stub.Request.State != "success" {
		t.Fatalf("posted status state = %q, want the green status recorded before the branch move", stub.Request.State)
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); !exists || got != moved {
		t.Fatalf("remote branch = (%q, %t), want the other commit %s untouched", got, exists, moved)
	}
}

func TestPublishGreenRefusesAnExpectedTipForABranchThatIsNotThere(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, nil)
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)

	_, _, err := publishGreen(
		capturePublicationTarget(dir),
		publicationBranchTarget{Branch: "feature", ExpectedTip: strings.Repeat("a", len(commit))},
		inspector.Result{Outcome: inspector.Green, Commit: commit},
		testJudgmentOptions{},
	)
	if err == nil || !strings.Contains(err.Error(), "does not exist on the remote") {
		t.Fatalf("error = %v, want a refusal naming the missing branch", err)
	}
	if stub.Request.State != "" {
		t.Fatalf("missing branch posted status state %q", stub.Request.State)
	}
	if _, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); exists {
		t.Fatal("a lease that could not hold created the branch anyway")
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

	_, _, err := publishGreen(capturePublicationTarget(dir), publicationBranchTarget{Branch: "feature"}, inspector.Result{Outcome: inspector.Green, Commit: commit}, testJudgmentOptions{})
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

	_, _, err := publishGreen(publicationTargetSnapshot{repoRoot: t.TempDir()}, publicationBranchTarget{Branch: "feature"}, inspector.Result{Outcome: inspector.Green, Commit: "abc"}, testJudgmentOptions{})
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

	_, _, err := publishGreen(capturePublicationTarget(dir), publicationBranchTarget{Branch: "main"}, inspector.Result{Outcome: inspector.Green, Commit: commit}, testJudgmentOptions{})
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

// TestCLI_GreenCheckCannotWriteTrackedCode is the end-to-end form of
// issue #27. The check command here has no reason to fail other than
// the write it attempts, so before #27 it would have passed and then
// been caught by #18's post-check comparison - which could only ever
// catch a change still visible when the run ended. Now the write itself
// fails inside the container, so the laundering attempt gets the check's
// own red and publishes nothing.
func TestCLI_GreenCheckCannotWriteTrackedCode(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		".inspector.json": `{"check": "printf changed > tracked.txt", "image": "alpine"}`,
		"tracked.txt":     "original",
	})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)

	code, stdout, stderr := runCLI(t, dir, "--branch", "feature")
	if code != exitRed {
		t.Fatalf("exit code = %d, want %d - the check's own failed write is a red check, not a refusal", code, exitRed)
	}
	if strings.Contains(stdout, "green -") {
		t.Fatalf("stdout = %q, must not bless a check that tried to rewrite its own source", stdout)
	}
	if got := readRepoFile(t, filepath.Join(dir, "tracked.txt")); got != "original" {
		t.Fatalf("tracked.txt = %q in the repository, want it untouched by the check", got)
	}
	// The operator has to be able to tell why their check failed.
	if !strings.Contains(strings.ToLower(stdout+stderr), "read-only file system") {
		t.Fatalf("stdout+stderr = %q, want the check's failure to name a read-only file system", stdout+stderr)
	}
	if stub.Request.State != "" {
		t.Fatalf("a check that tried to rewrite its own source posted status state %q", stub.Request.State)
	}
	if _, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); exists {
		t.Fatal("a check that tried to rewrite its own source published a staging ref")
	}
	if _, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); exists {
		t.Fatal("a check that tried to rewrite its own source published the named branch")
	}
}

// TestCLI_RedCheckCannotWriteTrackedCode is the same boundary for a
// check that would have failed anyway: the attempted write still never
// reaches the repository, and the operator still gets the check's own
// red rather than a refusal that says nothing about the code.
func TestCLI_RedCheckCannotWriteTrackedCode(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		".inspector.json": `{"check": "printf changed > tracked.txt; false", "image": "alpine"}`,
		"tracked.txt":     "original",
	})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)

	code, stdout, _ := runCLI(t, dir, "--branch", "feature")
	if code != exitRed {
		t.Fatalf("exit code = %d, want %d", code, exitRed)
	}
	if !strings.Contains(stdout, "red -") {
		t.Fatalf("stdout = %q, want the check's own red verdict", stdout)
	}
	if got := readRepoFile(t, filepath.Join(dir, "tracked.txt")); got != "original" {
		t.Fatalf("tracked.txt = %q in the repository, want it untouched by the check", got)
	}
	if stub.Request.State != "" {
		t.Fatalf("a red check posted status state %q", stub.Request.State)
	}
	if _, exists := remoteRef(t, stub.Remote, inspector.StagingRefForCommit(commit)); exists {
		t.Fatal("a red check published a staging ref")
	}
	if _, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); exists {
		t.Fatal("a red check published the named branch")
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

	_, _, err := publishGreen(snapshot, publicationBranchTarget{Branch: "feature"}, inspector.Result{Outcome: inspector.Green, Commit: commit}, testJudgmentOptions{})
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

	_, _, err := publishGreen(capturePublicationTarget(dir), publicationBranchTarget{Branch: "feature"}, inspector.Result{Outcome: inspector.Green, Commit: commit}, testJudgmentOptions{})
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

// writePRBodyFile writes body to a temp file and returns its path, for
// -pr-body-file in the pull-request tests below.
func writePRBodyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing pr body file: %v", err)
	}
	return path
}

func TestCLI_GreenWithPRFlagsOpensPullRequest(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	bodyPath := writePRBodyFile(t, "the login flow is done")

	code, stdout, _ := runCLI(t, dir, "--branch", "feature", "-pr-title", "Implement login", "-pr-body-file", bodyPath)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d", code, exitGreen)
	}
	if len(stub.PullRequests.Created) != 1 {
		t.Fatalf("created pull requests = %d, want 1: %+v", len(stub.PullRequests.Created), stub.PullRequests.Created)
	}
	pr := stub.PullRequests.Created[0]
	if pr.Head != "feature" || pr.Base != "main" || pr.Title != "Implement login" || pr.Body != "the login flow is done" {
		t.Fatalf("created pull request = %+v, want head=feature base=main title=%q body=%q", pr, "Implement login", "the login flow is done")
	}
	wantLine := "pull request: " + pr.URL
	if !strings.Contains(stdout, wantLine) {
		t.Fatalf("stdout = %q, want it to contain %q", stdout, wantLine)
	}
	latest, err := os.ReadFile(filepath.Join(dir, inspector.RunsDirName, inspector.LatestReportName))
	if err != nil {
		t.Fatalf("reading latest report: %v", err)
	}
	var report inspector.Report
	if err := json.Unmarshal(latest, &report); err != nil {
		t.Fatalf("decoding latest report: %v", err)
	}
	if report.PullRequestURL != pr.URL {
		t.Fatalf("report.PullRequestURL = %q, want %q", report.PullRequestURL, pr.URL)
	}
}

func TestCLI_GreenTwiceOnSameBranchOpensNoSecondPullRequest(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	bodyPath := writePRBodyFile(t, "the login flow is done")

	firstCode, _, _ := runCLI(t, dir, "--branch", "feature", "-pr-title", "Implement login", "-pr-body-file", bodyPath)
	if firstCode != exitGreen {
		t.Fatalf("first run exit code = %d, want %d", firstCode, exitGreen)
	}
	if len(stub.PullRequests.Created) != 1 {
		t.Fatalf("after first run, created pull requests = %d, want 1", len(stub.PullRequests.Created))
	}
	firstURL := stub.PullRequests.Created[0].URL

	runGitT(t, dir, "commit", "-q", "--allow-empty", "-m", "more work")

	secondCode, stdout, _ := runCLI(t, dir, "--branch", "feature", "-pr-title", "Implement login", "-pr-body-file", bodyPath)
	if secondCode != exitGreen {
		t.Fatalf("second run exit code = %d, want %d", secondCode, exitGreen)
	}
	if len(stub.PullRequests.Created) != 1 {
		t.Fatalf("after second run, created pull requests = %d, want still 1 (no second pull request)", len(stub.PullRequests.Created))
	}
	wantLine := "pull request: " + firstURL
	if !strings.Contains(stdout, wantLine) {
		t.Fatalf("second run stdout = %q, want it to report the existing pull request %q", stdout, wantLine)
	}
}

func TestCLI_RedWithPRFlagsOpensNoPullRequest(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "false", "image": "alpine"}`})
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	bodyPath := writePRBodyFile(t, "the login flow is done")

	code, stdout, _ := runCLI(t, dir, "--branch", "feature", "-pr-title", "Implement login", "-pr-body-file", bodyPath)
	if code != exitRed {
		t.Fatalf("exit code = %d, want %d", code, exitRed)
	}
	if len(stub.PullRequests.Created) != 0 {
		t.Fatalf("red result created %d pull requests, want 0", len(stub.PullRequests.Created))
	}
	if strings.Contains(stdout, "pull request:") {
		t.Fatalf("stdout = %q, a red result must never mention a pull request", stdout)
	}
}

func TestCLI_GreenWithoutPRFlagsOpensNoPullRequest(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{".inspector.json": `{"check": "true", "image": "alpine"}`})
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)

	code, stdout, _ := runCLI(t, dir, "--branch", "feature")
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d", code, exitGreen)
	}
	if len(stub.PullRequests.Created) != 0 || stub.PullRequests.ListRequests != 0 {
		t.Fatalf("green without pr flags made pull request calls: created=%d list=%d, want 0 and 0", len(stub.PullRequests.Created), stub.PullRequests.ListRequests)
	}
	if strings.Contains(stdout, "pull request:") {
		t.Fatalf("stdout = %q, must not mention a pull request when the flags are absent", stdout)
	}
}

func TestCLI_RefusesOnePRFlagWithoutTheOther(t *testing.T) {
	bodyPath := writePRBodyFile(t, "the login flow is done")

	t.Run("title without body file", func(t *testing.T) {
		dir := newTestRepo(t, nil)
		code, stdout, stderr := runCLI(t, dir, "--branch", "feature", "-pr-title", "Implement login")
		if code != exitRefused {
			t.Fatalf("exit code = %d, want %d", code, exitRefused)
		}
		if !strings.Contains(stderr, "-pr-body-file") {
			t.Fatalf("stderr = %q, want it to name the missing -pr-body-file", stderr)
		}
		if strings.Contains(stdout, "green") || strings.Contains(stdout, "red") {
			t.Fatalf("stdout = %q, must never run a check before this usage error is reported", stdout)
		}
	})

	t.Run("body file without title", func(t *testing.T) {
		dir := newTestRepo(t, nil)
		code, stdout, stderr := runCLI(t, dir, "--branch", "feature", "-pr-body-file", bodyPath)
		if code != exitRefused {
			t.Fatalf("exit code = %d, want %d", code, exitRefused)
		}
		if !strings.Contains(stderr, "-pr-title") {
			t.Fatalf("stderr = %q, want it to name the missing -pr-title", stderr)
		}
		if strings.Contains(stdout, "green") || strings.Contains(stdout, "red") {
			t.Fatalf("stdout = %q, must never run a check before this usage error is reported", stdout)
		}
	})
}
