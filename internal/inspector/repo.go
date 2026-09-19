package inspector

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
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
// Inspector needs this exact name only after it has recorded a green result:
// that is the branch a pull request can point at, so a detached HEAD has no
// safe publication target.
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

// PushRef sends source to origin under destination. Callers use an explicit
// source commit and destination ref rather than a tracking ref, so a checked
// commit is the only object a publication step can move.
func PushRef(repoRoot, source, destination string) error {
	if _, err := runGit(repoRoot, "push", "--porcelain", "origin", source+":"+destination); err != nil {
		return fmt.Errorf("pushing %s to %s: %w", source, destination, err)
	}
	return nil
}

// DeleteRemoteRef removes ref from origin after its staging purpose ends.
func DeleteRemoteRef(repoRoot, ref string) error {
	if _, err := runGit(repoRoot, "push", "--porcelain", "origin", ":"+ref); err != nil {
		return fmt.Errorf("deleting temporary staging ref %s: %w", ref, err)
	}
	return nil
}

// githubRemoteRE matches the URL shapes `git remote get-url` returns for
// a github.com remote: HTTPS (with or without a credential prefix or a
// trailing .git), the git@ scp-like form, and the ssh:// form.
var githubRemoteRE = regexp.MustCompile(`^(?:https?://(?:[^@/]+@)?github\.com/|(?:ssh://)?git@github\.com[:/])([^/]+)/(.+?)(?:\.git)?/?$`)

// RemoteOwnerRepo returns the GitHub owner and repository name parsed
// from the repo's "origin" remote, for posting a commit status against
// the right github.com/owner/repo.
func RemoteOwnerRepo(repoRoot string) (owner, repo string, err error) {
	out, err := runGit(repoRoot, "remote", "get-url", "origin")
	if err != nil {
		return "", "", fmt.Errorf("resolving the 'origin' remote: %w", err)
	}
	url := strings.TrimSpace(out)
	m := githubRemoteRE.FindStringSubmatch(url)
	if m == nil {
		return "", "", fmt.Errorf("origin remote %q is not a github.com URL inspector recognizes", redactURLCredentials(url))
	}
	return m[1], m[2], nil
}

// urlCredentialRE matches the userinfo part of a URL - everything between
// the scheme and the host.
var urlCredentialRE = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@]+@`)

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
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return string(out), nil
}
