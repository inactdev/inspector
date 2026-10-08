package inspector

import (
	"os"
	"os/exec"
	"path/filepath"
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

// newTestWorktree creates a repo with the given files committed, adds a
// linked worktree of it, and returns that worktree - the repo root
// inspector is actually handed in a factory where every worker works in
// one of these, and whose own .git is a pointer file naming a directory
// outside it (issue #31).
func newTestWorktree(t *testing.T, files map[string]string) string {
	t.Helper()
	project := newTestRepo(t, files)
	worktree := filepath.Join(t.TempDir(), "worktree")
	runGitT(t, project, "worktree", "add", "-q", "--detach", worktree)
	return worktree
}
