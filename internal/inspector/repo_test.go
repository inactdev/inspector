package inspector

import (
	"os"
	"os/exec"
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

func TestValidatePublicationBranch(t *testing.T) {
	for _, branch := range []string{"feature", "user/topic", "release-1.2"} {
		if err := ValidatePublicationBranch(branch); err != nil {
			t.Fatalf("ValidatePublicationBranch(%q): %v", branch, err)
		}
	}
	for _, branch := range []string{"", " main", "main ", "refs/heads/main", "bad..name", "-option"} {
		if err := ValidatePublicationBranch(branch); err == nil {
			t.Fatalf("ValidatePublicationBranch(%q) unexpectedly succeeded", branch)
		}
	}
}

func TestValidatePublicationBranchRejectsCheckoutShorthand(t *testing.T) {
	dir := newTestRepo(t, nil)
	runGitT(t, dir, "checkout", "-q", "-b", "previous")
	runGitT(t, dir, "checkout", "-q", "-b", "current")
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting current directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("changing to test repo: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDir); err != nil {
			t.Errorf("restoring current directory: %v", err)
		}
	})

	if err := ValidatePublicationBranch("@{-1}"); err == nil {
		t.Fatal("checkout shorthand unexpectedly accepted as a literal publication branch")
	}
}

func TestValidatePublicationCommit(t *testing.T) {
	dir := newTestRepo(t, map[string]string{"tracked.txt": "original"})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	policy, err := CaptureValidationPolicy(dir)
	if err != nil {
		t.Fatalf("capturing validation policy: %v", err)
	}

	if err := ValidatePublicationCommit(dir, commit, policy); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	writeFiles(t, dir, map[string]string{"tracked.txt": "changed"})
	if err := ValidatePublicationCommit(dir, commit, policy); err == nil || !strings.Contains(err.Error(), "tracked.txt") {
		t.Fatalf("error = %v, want changed tracked file to be named", err)
	}
	runGitT(t, dir, "reset", "--hard", "-q", "HEAD")
	writeFiles(t, dir, map[string]string{"tracked.txt": "staged"})
	runGitT(t, dir, "add", "tracked.txt")
	writeFiles(t, dir, map[string]string{"tracked.txt": "original"})
	if err := ValidatePublicationCommit(dir, commit, policy); err == nil || !strings.Contains(err.Error(), "tracked.txt") {
		t.Fatalf("error = %v, want staged tracked file to be named", err)
	}
	runGitT(t, dir, "reset", "--hard", "-q", "HEAD")
	writeFiles(t, dir, map[string]string{"untracked.txt": "new"})
	if err := ValidatePublicationCommit(dir, commit, policy); err == nil || !strings.Contains(err.Error(), "untracked.txt") {
		t.Fatalf("error = %v, want non-ignored untracked file to be named", err)
	}
	if err := os.Remove(filepath.Join(dir, "untracked.txt")); err != nil {
		t.Fatalf("removing untracked file: %v", err)
	}
	writeFiles(t, dir, map[string]string{".gitignore": "ignored.txt\n"})
	runGitT(t, dir, "add", ".gitignore")
	runGitT(t, dir, "commit", "-q", "-m", "ignore build output")
	commit = strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	policy, err = CaptureValidationPolicy(dir)
	if err != nil {
		t.Fatalf("recapturing validation policy: %v", err)
	}
	writeFiles(t, dir, map[string]string{"ignored.txt": "build output"})
	if err := ValidatePublicationCommit(dir, commit, policy); err != nil {
		t.Fatalf("ignored build output prevented publication: %v", err)
	}
	runGitT(t, dir, "commit", "--allow-empty", "-q", "-m", "other")
	if err := ValidatePublicationCommit(dir, commit, policy); err == nil {
		t.Fatal("expected changed HEAD to be rejected")
	}
}

func TestValidatePublicationCommitRejectsPolicyFromDifferentCommit(t *testing.T) {
	dir := newTestRepo(t, map[string]string{"tracked.txt": "original"})
	policy, err := CaptureValidationPolicy(dir)
	if err != nil {
		t.Fatalf("capturing validation policy: %v", err)
	}
	writeFiles(t, dir, map[string]string{"added.txt": "new commit"})
	runGitT(t, dir, "add", "added.txt")
	runGitT(t, dir, "commit", "-q", "-m", "move head")
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))

	err = ValidatePublicationCommit(dir, commit, policy)
	if err == nil || !strings.Contains(err.Error(), "captured for commit") {
		t.Fatalf("error = %v, want validation-state commit mismatch", err)
	}
}

func TestValidatePublicationCommitRejectsChangedSubmoduleWorktree(t *testing.T) {
	submodule := newTestRepo(t, map[string]string{"tracked.txt": "original"})
	dir := newTestRepo(t, nil)
	runGitT(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", submodule, "modules/child")
	runGitT(t, dir, "commit", "-q", "-m", "add submodule")
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	policy, err := CaptureValidationPolicy(dir)
	if err != nil {
		t.Fatalf("capturing validation policy: %v", err)
	}
	writeFiles(t, filepath.Join(dir, "modules", "child"), map[string]string{"tracked.txt": "changed by check"})

	err = ValidatePublicationCommit(dir, commit, policy)
	if err == nil || !strings.Contains(err.Error(), "modules/child/tracked.txt") {
		t.Fatalf("error = %v, want changed submodule file to be named", err)
	}
}

func TestValidatePublicationCommitUsesFrozenGitignoreFiles(t *testing.T) {
	dir := newTestRepo(t, nil)
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	policy, err := CaptureValidationPolicy(dir)
	if err != nil {
		t.Fatalf("capturing validation policy: %v", err)
	}
	writeFiles(t, dir, map[string]string{
		"generated/.gitignore": "*\n",
		"generated/input.txt":  "used by check",
	})

	err = ValidatePublicationCommit(dir, commit, policy)
	if err == nil || !strings.Contains(err.Error(), "generated/input.txt") {
		t.Fatalf("error = %v, want check-created ignore rules unable to hide additions", err)
	}
}

func TestValidatePublicationCommitRejectsMutableIndexFlags(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			dir := newTestRepo(t, map[string]string{"tracked.txt": "original"})
			commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
			policy, err := CaptureValidationPolicy(dir)
			if err != nil {
				t.Fatalf("capturing validation policy: %v", err)
			}
			runGitT(t, dir, "update-index", flag, "tracked.txt")
			writeFiles(t, dir, map[string]string{"tracked.txt": "changed"})

			err = ValidatePublicationCommit(dir, commit, policy)
			if err == nil || !strings.Contains(err.Error(), "tracked.txt") || !strings.Contains(err.Error(), "mutable index flag") {
				t.Fatalf("error = %v, want named mutable-index refusal", err)
			}
		})
	}
}

func TestValidatePublicationCommitDoesNotRunConfiguredFSMonitor(t *testing.T) {
	dir := newTestRepo(t, map[string]string{"tracked.txt": "original"})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	policy, err := CaptureValidationPolicy(dir)
	if err != nil {
		t.Fatalf("capturing validation policy: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	fsmonitor := filepath.Join(dir, ".git", "malicious-fsmonitor")
	script := "#!/bin/sh\nprintf '%s' \"$GITHUB_TOKEN\" > \"$FSMONITOR_MARKER\"\nprintf '2\\n'\n"
	if err := os.WriteFile(fsmonitor, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fsmonitor hook: %v", err)
	}
	runGitT(t, dir, "config", "core.fsmonitor", fsmonitor)
	t.Setenv(GitHubTokenEnvVar, "must-not-reach-validation")
	t.Setenv("FSMONITOR_MARKER", marker)
	writeFiles(t, dir, map[string]string{"tracked.txt": "changed"})

	err = ValidatePublicationCommit(dir, commit, policy)
	if err == nil || !strings.Contains(err.Error(), "tracked.txt") {
		t.Fatalf("error = %v, want independently detected tracked change", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("repository-configured fsmonitor ran during publication validation")
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking fsmonitor marker: %v", err)
	}
}

func TestValidatePublicationCommitPreservesCapturedIgnorePolicy(t *testing.T) {
	dir := newTestRepo(t, nil)
	infoExclude := filepath.Join(dir, ".git", "info", "exclude")
	if err := os.WriteFile(infoExclude, []byte("from-info.txt\n"), 0o644); err != nil {
		t.Fatalf("writing info exclude: %v", err)
	}
	globalExclude := filepath.Join(t.TempDir(), "global-ignore")
	if err := os.WriteFile(globalExclude, []byte("from-global.txt\n"), 0o644); err != nil {
		t.Fatalf("writing global exclude: %v", err)
	}
	runGitT(t, dir, "config", "core.excludesFile", globalExclude)

	policy, err := CaptureValidationPolicy(dir)
	if err != nil {
		t.Fatalf("capturing validation policy: %v", err)
	}
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	if err := os.WriteFile(infoExclude, []byte("changed-after-capture.txt\n"), 0o644); err != nil {
		t.Fatalf("changing info exclude: %v", err)
	}
	if err := os.WriteFile(globalExclude, []byte("changed-after-capture.txt\n"), 0o644); err != nil {
		t.Fatalf("changing global exclude: %v", err)
	}
	writeFiles(t, dir, map[string]string{
		"from-info.txt":             "ignored",
		"from-global.txt":           "ignored",
		"changed-after-capture.txt": "must remain visible",
	})

	err = ValidatePublicationCommit(dir, commit, policy)
	if err == nil || !strings.Contains(err.Error(), "changed-after-capture.txt") {
		t.Fatalf("error = %v, want post-capture ignore change to remain visible", err)
	}
	if strings.Contains(err.Error(), "from-info.txt") || strings.Contains(err.Error(), "from-global.txt") {
		t.Fatalf("error = %v, want captured ignore rules preserved", err)
	}
}

func TestFilteredCheckoutUsesFrozenWorktreeForPublicationValidation(t *testing.T) {
	dir := newTestRepo(t, nil)
	runGitT(t, dir, "config", "filter.snapshot.clean", "sed 's/^worktree$/stored/'")
	runGitT(t, dir, "config", "filter.snapshot.smudge", "sed 's/^stored$/worktree/'")
	writeFiles(t, dir, map[string]string{
		".gitattributes": "asset.txt filter=snapshot\n",
		"asset.txt":      "worktree\n",
	})
	runGitT(t, dir, "add", ".gitattributes", "asset.txt")
	runGitT(t, dir, "commit", "-q", "-m", "add filtered asset")

	status, err := WorkingTreeStatus(dir)
	if err != nil {
		t.Fatalf("checking filtered worktree: %v", err)
	}
	if status != "" {
		t.Fatalf("filtered checkout reported dirty: %q", status)
	}
	policy, err := CaptureValidationPolicy(dir)
	if err != nil {
		t.Fatalf("capturing filtered worktree: %v", err)
	}
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))

	marker := filepath.Join(t.TempDir(), "filter-ran")
	filter := filepath.Join(t.TempDir(), "clean-filter")
	if err := os.WriteFile(filter, []byte("#!/bin/sh\ntouch \"$FILTER_MARKER\"\nprintf 'stored\\n'\n"), 0o755); err != nil {
		t.Fatalf("writing replacement filter: %v", err)
	}
	t.Setenv("FILTER_MARKER", marker)
	runGitT(t, dir, "config", "filter.snapshot.clean", filter)

	if err := ValidatePublicationCommit(dir, commit, policy); err != nil {
		t.Fatalf("unchanged filtered checkout prevented publication: %v", err)
	}
	writeFiles(t, dir, map[string]string{"asset.txt": "changed after check\n"})
	err = ValidatePublicationCommit(dir, commit, policy)
	if err == nil || !strings.Contains(err.Error(), "asset.txt") {
		t.Fatalf("error = %v, want changed filtered file to be named", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("post-check validation executed the check-mutated clean filter")
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking filter marker: %v", err)
	}
}

func TestWorkingTreeStatusUsesRepositoryObjectFormat(t *testing.T) {
	dir := newTestRepo(t, map[string]string{"tracked.txt": "content"})
	t.Setenv("GIT_DEFAULT_HASH", "sha256")

	status, err := WorkingTreeStatus(dir)
	if err != nil {
		t.Fatalf("checking SHA-1 repository with ambient SHA-256 default: %v", err)
	}
	if status != "" {
		t.Fatalf("expected clean status, got %q", status)
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

func TestWorkingTreeStatus_IncludesTrackedRunsFile(t *testing.T) {
	dir := newTestRepo(t, map[string]string{RunsDirName + "/tracked.txt": "original"})
	writeFiles(t, dir, map[string]string{RunsDirName + "/tracked.txt": "changed"})

	status, err := WorkingTreeStatus(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(status, RunsDirName+"/tracked.txt") {
		t.Fatalf("status = %q, want tracked report-directory file", status)
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

func TestRemoteOwnerRepo_RejectsEmbeddedHTTPCredentials(t *testing.T) {
	dir := newTestRepo(t, nil)
	runGitT(t, dir, "remote", "add", "origin", "https://x-access-token:supersecret@github.com/inactdev/inspector.git")

	_, _, err := RemoteOwnerRepo(dir)
	if err == nil {
		t.Fatal("expected embedded HTTP credentials to be rejected")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error = %q, want the embedded credential omitted", err.Error())
	}
	if !strings.Contains(err.Error(), "credential helper or SSH") {
		t.Fatalf("error = %q, want safe authentication alternatives", err.Error())
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
func TestRemoteOwnerRepo_NotGitHubCredentialDoesNotLeak(t *testing.T) {
	dir := newTestRepo(t, nil)
	runGitT(t, dir, "remote", "add", "origin", "https://x-access-token:supersecret@gitlab.com/inactdev/inspector.git")

	_, _, err := RemoteOwnerRepo(dir)
	if err == nil {
		t.Fatal("expected credential-bearing remote to be rejected")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error = %q, want the embedded credential omitted", err.Error())
	}
	if !strings.Contains(err.Error(), "credential helper or SSH") {
		t.Fatalf("error = %q, want safe authentication alternatives", err.Error())
	}
}

func TestPushRefRejectsEmbeddedHTTPCredentialsBeforeStartingGit(t *testing.T) {
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "git-started")
	fakeGit := filepath.Join(binDir, "git")
	script := "#!/bin/sh\ntouch \"$GIT_STARTED_MARKER\"\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_STARTED_MARKER", marker)

	err := PushRefToRemote(t.TempDir(), "https://user:supersecret@github.com/owner/repo.git", "abc", "refs/heads/test")
	if err == nil || !strings.Contains(err.Error(), "credential helper or SSH") {
		t.Fatalf("error = %v, want embedded-credential refusal", err)
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error = %q, want credential omitted", err.Error())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git started with credentials in its process arguments")
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking git start marker: %v", err)
	}
}

func TestPushRefUsesNonInteractiveAuthentication(t *testing.T) {
	requirePublicationPlatform(t)
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	script := `#!/bin/sh
case "$*" in *"rev-parse --git-path objects"*) echo .git/objects; exit 0;; *"rev-parse --show-object-format=storage"*) echo sha1; exit 0;; esac
[ -z "$GITHUB_TOKEN" ] || exit 10
[ "$GIT_ASKPASS" = "true" ] || exit 11
[ "$GIT_TERMINAL_PROMPT" = "0" ] || exit 12
[ "$GCM_INTERACTIVE" = "Never" ] || exit 13
case "$GIT_SSH_COMMAND" in *BatchMode=yes*) exit 0 ;; *) exit 14 ;; esac
`
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(GitHubTokenEnvVar, "must-not-reach-git")

	if err := PushRefToRemote(t.TempDir(), "origin", "abc", "refs/heads/test"); err != nil {
		t.Fatalf("unexpected push error: %v", err)
	}
}

func TestPushRefDoesNotRunRepositoryHooks(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, map[string]string{"tracked.txt": "content"})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	remote := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "--quiet", remote).CombinedOutput(); err != nil {
		t.Fatalf("creating bare remote: %v\n%s", err, out)
	}
	hooksDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(hooksDir, "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch \"$HOOK_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("writing pre-push hook: %v", err)
	}
	runGitT(t, dir, "config", "core.hooksPath", hooksDir)
	t.Setenv("HOOK_MARKER", marker)
	t.Setenv(GitHubTokenEnvVar, "must-not-reach-git")

	if err := PushRefToRemote(dir, remote, commit, "refs/heads/feature"); err != nil {
		t.Fatalf("push from isolated publication repository failed: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("repository-local pre-push hook ran during inspector publication")
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking hook marker: %v", err)
	}
	cmd := exec.Command("git", "--git-dir", remote, "rev-parse", "--verify", "refs/heads/feature")
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != commit {
		t.Fatalf("remote branch = %q, %v; want %s", strings.TrimSpace(string(out)), err, commit)
	}
}

func TestPushRefUsesRepositoryObjectFormat(t *testing.T) {
	requirePublicationPlatform(t)
	dir := newTestRepo(t, map[string]string{"tracked.txt": "content"})
	commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	remote := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "--quiet", remote).CombinedOutput(); err != nil {
		t.Fatalf("creating bare remote: %v\n%s", err, out)
	}
	t.Setenv("GIT_DEFAULT_HASH", "sha256")

	if err := PushRefToRemote(dir, remote, commit, "refs/heads/feature"); err != nil {
		t.Fatalf("pushing SHA-1 repository with ambient SHA-256 default: %v", err)
	}
	cmd := exec.Command("git", "--git-dir", remote, "rev-parse", "--verify", "refs/heads/feature")
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != commit {
		t.Fatalf("remote branch = %q, %v; want %s", strings.TrimSpace(string(out)), err, commit)
	}
}

func TestPushRefTimeoutKillsChildProcesses(t *testing.T) {
	requirePublicationPlatform(t)
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	marker := filepath.Join(t.TempDir(), "survived")
	script := "#!/bin/sh\ncase \"$*\" in *\"rev-parse --git-path objects\"*) echo .git/objects; exit 0;; *\"rev-parse --show-object-format=storage\"*) echo sha1; exit 0;; *\"init --bare --quiet\"*) exit 0;; esac\n(sleep 0.3; touch \"$SURVIVAL_MARKER\") &\nwait\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SURVIVAL_MARKER", marker)
	previousTimeout := gitPushTimeout
	gitPushTimeout = 20 * time.Millisecond
	t.Cleanup(func() { gitPushTimeout = previousTimeout })

	err := PushRefToRemote(t.TempDir(), "origin", "abc", "refs/heads/test")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want push timeout", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git child survived the publication timeout")
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking child survival marker: %v", err)
	}
}

func TestPushRefRedactsCredentialsFromGitError(t *testing.T) {
	requirePublicationPlatform(t)
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	script := "#!/bin/sh\ncase \"$*\" in *\"rev-parse --git-path objects\"*) echo .git/objects; exit 0;; *\"rev-parse --show-object-format=storage\"*) echo sha1; exit 0;; *\"init --bare --quiet\"*) exit 0;; esac\necho 'fatal: https://user:supersecret@example.com/o/r.git rejected' >&2\nexit 1\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
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

func requirePublicationPlatform(t *testing.T) {
	t.Helper()
	if err := ValidatePublicationPlatform(); err != nil {
		t.Skipf("green publication is intentionally unavailable: %v", err)
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
