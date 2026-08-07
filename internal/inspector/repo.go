package inspector

import (
	"fmt"
	"os/exec"
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

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
