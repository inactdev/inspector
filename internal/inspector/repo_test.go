package inspector

import (
	"path/filepath"
	"strings"
	"testing"
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
