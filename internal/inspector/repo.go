package inspector

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ResolveRepoRoot returns the absolute top-level directory of the git
// working tree containing path.
func ResolveRepoRoot(path string) (string, error) {
	out, err := runGit(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository: %w", path, err)
	}
	return strings.TrimSpace(out), nil
}

// HeadCommit returns the full SHA of the repo's current HEAD commit - the
// exact commit any result produced by this run is bound to.
func HeadCommit(repoRoot string) (string, error) {
	out, err := runGit(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolving HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// CurrentBranch returns the local branch currently checked out at HEAD.
// A detached HEAD has no safe publication target.
func CurrentBranch(repoRoot string) (string, error) {
	out, err := runGit(repoRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolving the branch checked out at HEAD (inspector cannot publish from a detached HEAD): %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		return "", fmt.Errorf("resolving the branch checked out at HEAD: no branch name returned")
	}
	return branch, nil
}

// StagingRefForCommit names the non-branch ref that temporarily makes commit
// available to GitHub for its status post. It deliberately cannot be the
// pull-request branch: the status must exist before that branch moves.
func StagingRefForCommit(commit string) string {
	return "refs/inspector/staging/" + commit
}

// ValidatePublicationHead confirms the checked branch and commit are still
// checked out before publication starts.
func ValidatePublicationHead(repoRoot, expectedBranch, expectedCommit string) error {
	branch, err := CurrentBranch(repoRoot)
	if err != nil {
		return err
	}
	if branch != expectedBranch {
		return fmt.Errorf("checked-out branch changed during inspection: started on %q, now on %q", expectedBranch, branch)
	}
	commit, err := HeadCommit(repoRoot)
	if err != nil {
		return err
	}
	if commit != expectedCommit {
		return fmt.Errorf("HEAD changed during inspection: checked %s, now at %s", expectedCommit, commit)
	}
	return nil
}

var gitPushTimeout = 30 * time.Second

// PushRefToRemote sends source to the resolved publication remote. Callers use
// an explicit source commit and destination ref rather than a tracking ref, so
// a checked commit is the only object a publication step can move.
func PushRefToRemote(repoRoot, remote, source, destination string) error {
	if _, err := runGitPush(repoRoot, "push", "--porcelain", remote, source+":"+destination); err != nil {
		return fmt.Errorf("pushing %s to %s: %w", source, destination, err)
	}
	return nil
}

// DeleteRefFromRemote removes ref from the resolved publication remote.
func DeleteRefFromRemote(repoRoot, remote, ref string) error {
	if _, err := runGitPush(repoRoot, "push", "--porcelain", remote, ":"+ref); err != nil {
		return fmt.Errorf("deleting temporary staging ref %s: %w", ref, err)
	}
	return nil
}

// githubRemoteRE matches the URL shapes `git remote get-url` returns for
// a github.com remote: HTTPS (with or without a credential prefix or a
// trailing .git), the git@ scp-like form, and the ssh:// form.
var githubRemoteRE = regexp.MustCompile(`^(?:https?://(?:[^@/]+@)?github\.com/|(?:ssh://)?git@github\.com[:/])([^/]+)/(.+?)(?:\.git)?/?$`)

// PublicationTarget binds the exact push URL to the GitHub repository that
// receives the commit status.
type PublicationTarget struct {
	PushURL     string
	Owner, Repo string
}

// ResolvePublicationTarget resolves origin's effective push URL once so every
// push and the status post use one immutable publication target.
func ResolvePublicationTarget(repoRoot string) (PublicationTarget, error) {
	out, err := runGit(repoRoot, "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return PublicationTarget{}, fmt.Errorf("resolving the 'origin' push remote: %w", err)
	}
	urls := strings.Split(strings.TrimSpace(out), "\n")
	if len(urls) != 1 || strings.TrimSpace(urls[0]) == "" {
		return PublicationTarget{}, fmt.Errorf("origin must have exactly one push URL so refs and status cannot be split across repositories; found %d", len(urls))
	}
	url := strings.TrimSpace(urls[0])
	m := githubRemoteRE.FindStringSubmatch(url)
	if m == nil {
		return PublicationTarget{}, fmt.Errorf("origin push remote %q is not a github.com URL inspector recognizes", redactURLCredentials(url))
	}
	return PublicationTarget{PushURL: url, Owner: m[1], Repo: m[2]}, nil
}

// RemoteOwnerRepo returns the GitHub owner and repository name for origin's
// publication target.
func RemoteOwnerRepo(repoRoot string) (owner, repo string, err error) {
	target, err := ResolvePublicationTarget(repoRoot)
	if err != nil {
		return "", "", err
	}
	return target.Owner, target.Repo, nil
}

// urlCredentialRE matches the userinfo part of a URL - everything between
// the scheme and the host.
var urlCredentialRE = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]+@`)

// redactURLCredentials strips any embedded credentials from a remote URL
// before it goes into an error message. inspector's errors land in CI and
// pipeline logs, and a remote like https://user:token@example.com/o/r.git
// would otherwise write that token there.
func redactURLCredentials(url string) string {
	return urlCredentialRE.ReplaceAllString(url, "${1}[redacted]@")
}

// WorkingTreeStatus returns the raw `git status --porcelain` output,
// excluding RunsDirName - inspector's own report directory. Without the
// exclusion, a repo that never gitignores RunsDirName would go dirty the
// moment inspector wrote its first report, permanently refusing every
// run after. A non-empty result means the working tree does not
// otherwise match HEAD, so a check run against it cannot honestly be
// bound to that commit.
func WorkingTreeStatus(repoRoot string) (string, error) {
	out, err := runGit(repoRoot, "status", "--porcelain", "--", ".", ":!"+RunsDirName)
	if err != nil {
		return "", fmt.Errorf("checking working tree status: %w", err)
	}
	return out, nil
}

// runGit returns only git's stdout. Its stderr is kept separate and used
// solely for the error message - folding it into the value would let a
// warning git prints on an otherwise successful command be parsed as a
// commit SHA or as a dirty working tree.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return gitOutput(cmd, args)
}

func runGitPush(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitPushTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.WaitDelay = time.Second
	cmd.Env = nonInteractiveGitEnv()
	out, err := gitOutput(cmd, args)
	if ctx.Err() == context.DeadlineExceeded {
		command := redactURLCredentials(strings.Join(args, " "))
		return "", fmt.Errorf("git %s timed out after %s", command, gitPushTimeout)
	}
	return out, err
}

func gitOutput(cmd *exec.Cmd, args []string) (string, error) {
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		command := redactURLCredentials(strings.Join(args, " "))
		detail := redactURLCredentials(strings.TrimSpace(errBuf.String()))
		return "", fmt.Errorf("git %s: %w: %s", command, err, detail)
	}
	return string(out), nil
}

func nonInteractiveGitEnv() []string {
	blocked := map[string]bool{
		"GIT_ASKPASS":         true,
		"GIT_SSH_COMMAND":     true,
		"GIT_TERMINAL_PROMPT": true,
		"GCM_INTERACTIVE":     true,
	}
	env := make([]string, 0, len(os.Environ())+4)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if !blocked[name] {
			env = append(env, item)
		}
	}
	return append(env,
		"GIT_ASKPASS=true",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=Never",
	)
}
