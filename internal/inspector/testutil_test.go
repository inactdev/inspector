package inspector

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// defaultTestDockerfile is the check image a test repo gets when the test
// does not name its own. Every project inspector judges has to commit a
// ProjectDockerfileName for its check command to run in (issue #47), and
// almost none of the tests below is about which image that is - so this
// keeps them stating only what they are actually testing. The few that
// care pass their own, and the one that tests its absence removes it.
const defaultTestDockerfile = "FROM " + testImage + "\n"

// newTestRepo creates a fresh git repo in a temp dir with one commit
// containing the given files - plus a default ProjectDockerfileName when
// the caller named none - and returns the repo root.
func newTestRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	return newTestRepoIn(t, t.TempDir(), files)
}

// newTestRepoIn is newTestRepo at a path the caller chose, for the tests
// that care where the repository sits relative to something else - a
// terms folder that must not hold it, for instance (issue #69).
func newTestRepoIn(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}

	runGitT(t, dir, "init", "-q")
	runGitT(t, dir, "config", "user.email", "test@example.com")
	runGitT(t, dir, "config", "user.name", "Test")

	if _, named := files[ProjectDockerfileName]; !named {
		withDockerfile := make(map[string]string, len(files)+1)
		for name, content := range files {
			withDockerfile[name] = content
		}
		withDockerfile[ProjectDockerfileName] = defaultTestDockerfile
		files = withDockerfile
	}
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
