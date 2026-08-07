package inspector

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRepo creates a fresh git repo in a temp dir with one commit
// containing the given files, and returns the repo root.
func newTestRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	runGitT(t, dir, "init", "-q")
	runGitT(t, dir, "config", "user.email", "test@example.com")
	runGitT(t, dir, "config", "user.name", "Test")

	writeFiles(t, dir, files)

	runGitT(t, dir, "add", "-A")
	runGitT(t, dir, "commit", "-q", "-m", "initial", "--allow-empty")

	return dir
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// The well-known SHA of git's empty tree object - a git-wide constant,
// always considered valid even when never written to a given repo's
// object database.
const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// ambiguousCommitA and ambiguousCommitB are two commit SHAs that share
// the 4-character prefix "bd19". Commit hashing is pure and
// OS-independent, so committing the empty tree with a fixed identity,
// fixed dates, and these exact messages reproduces the same two SHAs on
// any machine - found once offline by brute-force search over candidate
// messages, not derived from anything else. makeAmbiguousCommits
// recreates them without needing to search again.
const (
	ambiguousPrefix   = "bd19"
	ambiguousMessageA = "seed-12"
	ambiguousMessageB = "seed-1071"
	ambiguousCommitA  = "bd19567cc0185beac1dd83558f860b9bd53498b9"
	ambiguousCommitB  = "bd19580b8b9f44d476486c901ced6007d17f8ddc"
)

// makeAmbiguousCommits creates ambiguousCommitA and ambiguousCommitB in
// dir's object database (via `git commit-tree`, so neither needs to be
// reachable from any ref) and points dir's current branch at
// ambiguousCommitA - an empty-tree commit, so the working directory
// needs no files to read as clean. It returns the two SHAs.
func makeAmbiguousCommits(t *testing.T, dir string) (commitA, commitB string) {
	t.Helper()
	commitA = commitTreeT(t, dir, ambiguousMessageA)
	commitB = commitTreeT(t, dir, ambiguousMessageB)
	if commitA != ambiguousCommitA || commitB != ambiguousCommitB {
		t.Fatalf("git commit-tree produced unexpected SHAs (%s, %s) - git's commit hashing changed underneath this fixture", commitA, commitB)
	}

	branch := strings.TrimSpace(runGitT(t, dir, "symbolic-ref", "--short", "HEAD"))
	runGitT(t, dir, "update-ref", "refs/heads/"+branch, commitA)
	return commitA, commitB
}

func commitTreeT(t *testing.T, dir, message string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "commit-tree", emptyTreeSHA, "-m", message)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=inspector-test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME=inspector-test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git commit-tree: %v", err)
	}
	return strings.TrimSpace(string(out))
}
