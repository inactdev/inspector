package inspector

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// StagingRefForCommit names the non-branch ref that temporarily makes commit
// available to GitHub for its status post. It deliberately cannot be the
// pull-request branch: the status must exist before that branch moves.
func StagingRefForCommit(commit string) string {
	return "refs/inspector/staging/" + commit
}

// ValidatePublicationBranch accepts only a full, unambiguous branch name.
func ValidatePublicationBranch(branch string) error {
	if branch == "" || strings.TrimSpace(branch) != branch {
		return fmt.Errorf("branch name must be non-empty and contain no leading or trailing whitespace")
	}
	if strings.HasPrefix(branch, "refs/") {
		return fmt.Errorf("branch name %q must not include a refs/ prefix", branch)
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("branch name %q must not begin with a hyphen", branch)
	}
	if _, err := runGit(".", "check-ref-format", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("%q is not a valid git branch name: %w", branch, err)
	}
	return nil
}

// ValidatePublicationCommit confirms the checked commit is still HEAD and the
// working tree still contains exactly that commit's code.
func ValidatePublicationCommit(repoRoot, expectedCommit string) error {
	commit, err := HeadCommit(repoRoot)
	if err != nil {
		return err
	}
	if commit != expectedCommit {
		return fmt.Errorf("HEAD changed during inspection: checked %s, now at %s", expectedCommit, commit)
	}
	status, err := WorkingTreeStatus(repoRoot)
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("working tree changed during inspection, so the passing check was not run against commit %s exactly; commit or stash these files before retrying:\n\n%s", expectedCommit, status)
	}
	return nil
}

var gitPushTimeout = 30 * time.Second

// ValidatePublicationPlatform refuses platforms where a timed-out push cannot
// reliably terminate git's complete process tree.
func ValidatePublicationPlatform() error {
	return validatePublicationPlatform()
}

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
var httpURLCredentialRE = regexp.MustCompile(`(?i)^https?://[^/@\s]+@`)

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
	pushURL := strings.TrimSpace(urls[0])
	if hasEmbeddedHTTPCredentials(pushURL) {
		return PublicationTarget{}, fmt.Errorf("origin push remote contains embedded HTTP credentials; use a credential helper or SSH instead")
	}
	m := githubRemoteRE.FindStringSubmatch(pushURL)
	if m == nil {
		return PublicationTarget{}, fmt.Errorf("origin push remote %q is not a github.com URL inspector recognizes", redactURLCredentials(pushURL))
	}
	return PublicationTarget{PushURL: pushURL, Owner: m[1], Repo: m[2]}, nil
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

func hasEmbeddedHTTPCredentials(value string) bool {
	return httpURLCredentialRE.MatchString(value)
}

// WorkingTreeStatus returns `git status --porcelain` output plus any mutable
// index flags that could hide tracked changes. It omits untracked files in
// RunsDirName, inspector's own report directory, but still reports tracked
// changes there. Without the untracked exclusion, a repo that never gitignores
// RunsDirName would go dirty after its first report. A non-empty result means
// the working tree cannot be proven to match HEAD, so a check run against it
// cannot honestly be bound to that commit.
func WorkingTreeStatus(repoRoot string) (string, error) {
	out, err := runGit(repoRoot, "status", "--porcelain", "--untracked-files=all", "--", ".", ":!"+RunsDirName)
	if err != nil {
		return "", fmt.Errorf("checking working tree status: %w", err)
	}
	trackedRuns, err := runGit(repoRoot, "status", "--porcelain", "--untracked-files=no", "--", RunsDirName)
	if err != nil {
		return "", fmt.Errorf("checking tracked report files: %w", err)
	}
	mutableIndex, err := mutableIndexFlags(repoRoot)
	if err != nil {
		return "", err
	}
	return out + trackedRuns + mutableIndex, nil
}

func mutableIndexFlags(repoRoot string) (string, error) {
	out, err := runGit(repoRoot, "ls-files", "-v", "-z")
	if err != nil {
		return "", fmt.Errorf("checking mutable index flags: %w", err)
	}
	var flagged strings.Builder
	for _, entry := range strings.Split(out, "\x00") {
		if len(entry) < 3 || entry[1] != ' ' {
			continue
		}
		if entry[0] == 'S' || entry[0] >= 'a' && entry[0] <= 'z' {
			fmt.Fprintf(&flagged, "mutable index flag hides tracked content: %s\n", entry[2:])
		}
	}
	return flagged.String(), nil
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
	for _, arg := range args {
		if hasEmbeddedHTTPCredentials(arg) {
			return "", fmt.Errorf("git push target contains embedded HTTP credentials; use a credential helper or SSH instead")
		}
	}
	if err := ValidatePublicationPlatform(); err != nil {
		return "", err
	}

	objectDir, err := gitObjectDirectory(dir)
	if err != nil {
		return "", err
	}
	publicationRepo, err := os.MkdirTemp("", "inspector-publication-")
	if err != nil {
		return "", fmt.Errorf("creating isolated publication repository: %w", err)
	}
	defer os.RemoveAll(publicationRepo)

	env := publicationGitEnv(objectDir)
	initCmd := exec.Command("git", "init", "--bare", "--quiet", publicationRepo)
	initCmd.Env = env
	if _, err := gitOutput(initCmd, []string{"init", "--bare", "--quiet", publicationRepo}); err != nil {
		return "", fmt.Errorf("creating isolated publication repository: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitPushTimeout)
	defer cancel()

	gitArgs := append([]string{"--git-dir=" + publicationRepo, "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = time.Second
	cmd.Env = env
	out, err := gitOutput(cmd, args)
	if ctx.Err() == context.DeadlineExceeded {
		command := redactURLCredentials(strings.Join(args, " "))
		return "", fmt.Errorf("git %s timed out after %s", command, gitPushTimeout)
	}
	return out, err
}

func gitObjectDirectory(repoRoot string) (string, error) {
	out, err := runGit(repoRoot, "rev-parse", "--git-path", "objects")
	if err != nil {
		return "", fmt.Errorf("resolving repository object directory: %w", err)
	}
	path := strings.TrimSpace(out)
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	return filepath.Clean(path), nil
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
		"GITHUB_TOKEN":                     true,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_ASKPASS":                      true,
		"GIT_COMMON_DIR":                   true,
		"GIT_CONFIG_COUNT":                 true,
		"GIT_CONFIG_PARAMETERS":            true,
		"GIT_DIR":                          true,
		"GIT_OBJECT_DIRECTORY":             true,
		"GIT_SSH_COMMAND":                  true,
		"GIT_TERMINAL_PROMPT":              true,
		"GIT_WORK_TREE":                    true,
		"GCM_INTERACTIVE":                  true,
	}
	env := make([]string, 0, len(os.Environ())+4)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if !blocked[name] && !strings.HasPrefix(name, "GIT_CONFIG_KEY_") && !strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
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

func publicationGitEnv(objectDir string) []string {
	return append(nonInteractiveGitEnv(), "GIT_ALTERNATE_OBJECT_DIRECTORIES="+objectDir)
}
