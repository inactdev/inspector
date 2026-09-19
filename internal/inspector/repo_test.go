package inspector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveRepoRoot(t *testing.T) {
	dir := newTestRepo(t, nil)

	root, err := ResolveRepoRoot(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.EqualFold(root, resolveSymlinks(t, dir)) {
		t.Fatalf("root = %q, want %q", root, dir)
	}
}

func TestResolveRepoRoot_NotARepo(t *testing.T) {
	dir := t.TempDir()

	if _, err := ResolveRepoRoot(dir); err == nil {
		t.Fatal("expected an error for a non-repo path")
	}
}

func TestHeadCommit(t *testing.T) {
	dir := newTestRepo(t, map[string]string{"a.txt": "hello"})

	got, err := HeadCommit(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	if got != want {
		t.Fatalf("HeadCommit = %q, want %q", got, want)
	}
}

func TestCurrentBranch(t *testing.T) {
	dir := newTestRepo(t, nil)

	got, err := CurrentBranch(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := strings.TrimSpace(runGitT(t, dir, "branch", "--show-current"))
	if got != want {
		t.Fatalf("CurrentBranch = %q, want %q", got, want)
	}
}

func TestCurrentBranch_RefusesDetachedHead(t *testing.T) {
	dir := newTestRepo(t, nil)
	runGitT(t, dir, "checkout", "--detach", "-q")

	if _, err := CurrentBranch(dir); err == nil {
		t.Fatal("expected a detached HEAD to have no publication branch")
	}
}

func TestValidatePublicationHead(t *testing.T) {
	dir := newTestRepo(t, nil)
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	branch := strings.TrimSpace(runGitT(t, dir, "branch", "--show-current"))

	if err := ValidatePublicationHead(dir, branch, commit); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runGitT(t, dir, "checkout", "-q", "-b", "other")
	if err := ValidatePublicationHead(dir, branch, commit); err == nil {
		t.Fatal("expected changed branch to be rejected")
	}
}

func TestWorkingTreeStatus_Clean(t *testing.T) {
	dir := newTestRepo(t, map[string]string{"a.txt": "hello"})

	status, err := WorkingTreeStatus(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "" {
		t.Fatalf("expected clean status, got %q", status)
	}
}

func TestWorkingTreeStatus_UntrackedFile(t *testing.T) {
	dir := newTestRepo(t, nil)
	writeFiles(t, dir, map[string]string{"stray.txt": "oops"})

	status, err := WorkingTreeStatus(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status == "" {
		t.Fatal("expected a dirty status for an untracked file")
	}
}

func TestWorkingTreeStatus_IgnoresOwnRunsDir(t *testing.T) {
	dir := newTestRepo(t, map[string]string{"a.txt": "hello"})
	writeFiles(t, dir, map[string]string{
		RunsDirName + "/runs/report.json":    `{"outcome":"green"}`,
		RunsDirName + "/" + LatestReportName: `{"outcome":"green"}`,
	})

	status, err := WorkingTreeStatus(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "" {
		t.Fatalf("expected inspector's own report dir to be excluded, got %q", status)
	}
}

func TestWorkingTreeStatus_ModifiedFile(t *testing.T) {
	dir := newTestRepo(t, map[string]string{"a.txt": "hello"})
	writeFiles(t, dir, map[string]string{"a.txt": "changed"})

	status, err := WorkingTreeStatus(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status == "" {
		t.Fatal("expected a dirty status for a modified tracked file")
	}
}

func TestRemoteOwnerRepo(t *testing.T) {
	cases := []struct {
		name, url, wantOwner, wantRepo string
	}{
		{"https", "https://github.com/inactdev/inspector.git", "inactdev", "inspector"},
		{"https no dot git", "https://github.com/inactdev/inspector", "inactdev", "inspector"},
		{"https with credential", "https://x-access-token:abc123@github.com/inactdev/inspector.git", "inactdev", "inspector"},
		{"scp-like ssh", "git@github.com:inactdev/inspector.git", "inactdev", "inspector"},
		{"ssh url", "ssh://git@github.com/inactdev/inspector.git", "inactdev", "inspector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := newTestRepo(t, nil)
			runGitT(t, dir, "remote", "add", "origin", tc.url)

			owner, repo, err := RemoteOwnerRepo(dir)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Fatalf("RemoteOwnerRepo(%q) = (%q, %q), want (%q, %q)", tc.url, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

func TestRemoteOwnerRepo_UsesPushTarget(t *testing.T) {
	dir := newTestRepo(t, nil)
	runGitT(t, dir, "remote", "add", "origin", "https://github.com/fetch-owner/fetch-repo.git")
	runGitT(t, dir, "remote", "set-url", "--push", "origin", "https://github.com/push-owner/push-repo.git")

	owner, repo, err := RemoteOwnerRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "push-owner" || repo != "push-repo" {
		t.Fatalf("RemoteOwnerRepo = (%q, %q), want push target (%q, %q)", owner, repo, "push-owner", "push-repo")
	}
}

func TestRemoteOwnerRepo_RejectsMultiplePushTargets(t *testing.T) {
	dir := newTestRepo(t, nil)
	runGitT(t, dir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runGitT(t, dir, "remote", "set-url", "--add", "--push", "origin", "https://github.com/owner/first.git")
	runGitT(t, dir, "remote", "set-url", "--add", "--push", "origin", "https://github.com/owner/second.git")

	if _, _, err := RemoteOwnerRepo(dir); err == nil {
		t.Fatal("expected multiple push targets to be rejected")
	}
}

func TestRemoteOwnerRepo_NoOrigin(t *testing.T) {
	dir := newTestRepo(t, nil)

	if _, _, err := RemoteOwnerRepo(dir); err == nil {
		t.Fatal("expected an error with no origin remote configured")
	}
}

func TestRemoteOwnerRepo_NotGitHub(t *testing.T) {
	dir := newTestRepo(t, nil)
	runGitT(t, dir, "remote", "add", "origin", "https://gitlab.com/inactdev/inspector.git")

	if _, _, err := RemoteOwnerRepo(dir); err == nil {
		t.Fatal("expected an error for a non-github.com remote")
	}
}

// The unrecognized-remote error goes to stderr, which for this tool ends
// up in CI and pipeline logs, so a credential embedded in the URL must
// not travel with it.
func TestRemoteOwnerRepo_NotGitHubRedactsCredentials(t *testing.T) {
	dir := newTestRepo(t, nil)
	runGitT(t, dir, "remote", "add", "origin", "https://x-access-token:supersecret@gitlab.com/inactdev/inspector.git")

	_, _, err := RemoteOwnerRepo(dir)
	if err == nil {
		t.Fatal("expected an error for a non-github.com remote")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error = %q, want the embedded credential redacted", err.Error())
	}
	if !strings.Contains(err.Error(), "gitlab.com") {
		t.Fatalf("error = %q, want it to still name the remote host", err.Error())
	}
}

func TestPushRefUsesNonInteractiveAuthentication(t *testing.T) {
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	script := `#!/bin/sh
[ "$GIT_ASKPASS" = "true" ] || exit 11
[ "$GIT_TERMINAL_PROMPT" = "0" ] || exit 12
[ "$GCM_INTERACTIVE" = "Never" ] || exit 13
case "$GIT_SSH_COMMAND" in *BatchMode=yes*) exit 0 ;; *) exit 14 ;; esac
`
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := PushRefToRemote(t.TempDir(), "origin", "abc", "refs/heads/test"); err != nil {
		t.Fatalf("unexpected push error: %v", err)
	}
}

func TestPushRefTimesOut(t *testing.T) {
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatalf("writing fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	previousTimeout := gitPushTimeout
	gitPushTimeout = 20 * time.Millisecond
	t.Cleanup(func() { gitPushTimeout = previousTimeout })

	err := PushRefToRemote(t.TempDir(), "origin", "abc", "refs/heads/test")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want push timeout", err)
	}
}

func TestPushRefRedactsCredentialsFromGitError(t *testing.T) {
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\necho 'fatal: https://user:supersecret@example.com/o/r.git rejected' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("writing fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := PushRefToRemote(t.TempDir(), "origin", "abc", "refs/heads/test")
	if err == nil {
		t.Fatal("expected push failure")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error = %q, want the embedded credential redacted", err.Error())
	}
	if !strings.Contains(err.Error(), "example.com") {
		t.Fatalf("error = %q, want the remote host preserved", err.Error())
	}
}

// resolveSymlinks lets the root comparison tolerate macOS's /tmp ->
// /private/tmp symlink, which `git rev-parse --show-toplevel` resolves.
func resolveSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolving symlinks for %s: %v", path, err)
	}
	return resolved
}
