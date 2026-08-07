package inspector

import (
	"bytes"
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

// ResolveCommit resolves ref to a full commit SHA the way git itself
// would - accepting any form git accepts unambiguously, including a
// short SHA prefix, a branch, or a tag - via `git rev-parse --verify`.
// It errors if ref does not name exactly one commit: nonexistent and
// ambiguous (a short prefix matching more than one object) both fail
// here, with git's own message identifying which.
func ResolveCommit(repoRoot, ref string) (string, error) {
	out, err := runGit(repoRoot, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
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
