package container

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newRepoLayout builds the shape of a repo root ResolveWorkspace has to
// take apart: tracked source, a tracked file, a gitignored directory
// left behind by an earlier run, and a .git. Pure filesystem - what git
// actually tracks is the caller's answer, not this function's.
func newRepoLayout(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	for _, sub := range []string{"cmd", "node_modules", ".git"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("creating %s: %v", sub, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	return dir
}

func TestResolveWorkspace_TrackedSourceIsReadOnlyAndTheRestIsNot(t *testing.T) {
	dir := newRepoLayout(t)

	workspace, err := ResolveWorkspace(dir, []string{"cmd", "go.mod"})
	if err != nil {
		t.Fatalf("ResolveWorkspace(%q) = %v, want the mounts a check command runs against", dir, err)
	}
	defer workspace.Cleanup()

	readOnly := map[string]string{}
	for _, mount := range workspace.SourceMounts {
		readOnly[mount.Target] = mount.Source
	}
	writable := map[string]string{}
	for _, mount := range workspace.WritableMounts {
		writable[mount.Target] = mount.Source
	}
	// .git is read-only alongside tracked source without being tracked:
	// it is the commit under check, not a leftover.
	for _, name := range []string{"cmd", "go.mod", ".git"} {
		target := WorkspaceDir + "/" + name
		if readOnly[target] != filepath.Join(dir, name) {
			t.Fatalf("%s is not mounted read-only from the repo: SourceMounts = %v", name, workspace.SourceMounts)
		}
		if _, ok := writable[target]; ok {
			t.Fatalf("%s is mounted writable as well as read-only: WritableMounts = %v", name, workspace.WritableMounts)
		}
	}
	if writable[WorkspaceDir+"/node_modules"] != filepath.Join(dir, "node_modules") {
		t.Fatalf("an untracked, gitignored path is not mounted writable: WritableMounts = %v", workspace.WritableMounts)
	}
}

func TestResolveWorkspace_WorkspaceRootIsWritableWithMountPointsForEveryEntry(t *testing.T) {
	dir := newRepoLayout(t)

	workspace, err := ResolveWorkspace(dir, []string{"cmd", "go.mod"})
	if err != nil {
		t.Fatalf("ResolveWorkspace(%q) = %v, want the mounts a check command runs against", dir, err)
	}
	defer workspace.Cleanup()

	// Docker needs a mount point of the matching kind at each target,
	// and the root itself has to stay writable - that is what lets a
	// check create dist/ or node_modules in its own tree with no
	// project configuration at all.
	for name, wantDir := range map[string]bool{"cmd": true, ".git": true, "node_modules": true, "go.mod": false} {
		info, err := os.Lstat(filepath.Join(workspace.Root, name))
		if err != nil {
			t.Fatalf("no mount point for %s in the workspace: %v", name, err)
		}
		if info.IsDir() != wantDir {
			t.Fatalf("mount point for %s is a directory = %v, want %v", name, info.IsDir(), wantDir)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace.Root, "dist"), []byte("built"), 0o644); err != nil {
		t.Fatalf("the workspace root is not writable: %v", err)
	}
}

func TestResolveWorkspace_CleanupDiscardsEverythingTheCheckWrote(t *testing.T) {
	dir := newRepoLayout(t)

	workspace, err := ResolveWorkspace(dir, []string{"cmd", "go.mod"})
	if err != nil {
		t.Fatalf("ResolveWorkspace(%q) = %v, want the mounts a check command runs against", dir, err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Root, "coverage.out"), []byte("mode: set\n"), 0o644); err != nil {
		t.Fatalf("writing into the workspace: %v", err)
	}

	workspace.Cleanup()

	if _, err := os.Stat(workspace.Root); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%q) = %v after Cleanup, want it gone", workspace.Root, err)
	}
	workspace.Cleanup()
	Workspace{}.Cleanup()
}

func TestResolveWorkspace_CarriesAnInRepoSymlinkAsALink(t *testing.T) {
	// A symlink cannot be mounted - Docker resolves the source path -
	// and resolving an in-repo one would hand the container a second
	// real copy of something the commit has once. So it is recreated as
	// a link, pointing at the same place relative to the root. This
	// repository's own CLAUDE.md -> AGENTS.md is exactly this case.
	dir := newRepoLayout(t)
	if err := os.Symlink("go.mod", filepath.Join(dir, "AGENT.md")); err != nil {
		t.Skipf("cannot create a symlink here, skipping: %v", err)
	}

	workspace, err := ResolveWorkspace(dir, []string{"cmd", "go.mod", "AGENT.md"})
	if err != nil {
		t.Fatalf("ResolveWorkspace(repo with an in-repo root symlink) = %v, want it carried", err)
	}
	defer workspace.Cleanup()

	target, err := os.Readlink(filepath.Join(workspace.Root, "AGENT.md"))
	if err != nil {
		t.Fatalf("the in-repo symlink was not recreated in the workspace: %v", err)
	}
	// git recorded "go.mod", so that is what a check running `git
	// status` in there has to find.
	if target != "go.mod" {
		t.Fatalf("recreated link points at %q, want the link text git recorded (%q)", target, "go.mod")
	}
	for _, mount := range append(workspace.SourceMounts, workspace.WritableMounts...) {
		if mount.Target == WorkspaceDir+"/AGENT.md" {
			t.Fatalf("the symlink was mounted as %v, which would resolve it on the host", mount)
		}
	}
}

func TestResolveWorkspace_RefusesASymlinkLeadingOutOfTheRepo(t *testing.T) {
	// Honoring it would mean mounting a host path the commit under
	// check does not contain, which is the containment bar issue #13
	// set. Refused by name rather than dropped or followed.
	dir := newRepoLayout(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "config")); err != nil {
		t.Skipf("cannot create a symlink here, skipping: %v", err)
	}

	workspace, err := ResolveWorkspace(dir, []string{"cmd", "go.mod", "config"})
	defer workspace.Cleanup()

	if !errors.Is(err, ErrWorkspaceUnavailable) {
		t.Fatalf("ResolveWorkspace(repo with a root symlink out of the repo) = %v, want ErrWorkspaceUnavailable", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "config")) {
		t.Fatalf("error = %q, want it to name the symlink", err)
	}
	if workspace.Root != "" {
		t.Fatalf("a refused ResolveWorkspace returned workspace root %q, want nothing left behind", workspace.Root)
	}
}

func TestResolveWorkspace_RefusesADanglingSymlink(t *testing.T) {
	dir := newRepoLayout(t)
	if err := os.Symlink("gone", filepath.Join(dir, "config")); err != nil {
		t.Skipf("cannot create a symlink here, skipping: %v", err)
	}

	_, err := ResolveWorkspace(dir, []string{"cmd", "go.mod", "config"})

	if !errors.Is(err, ErrWorkspaceUnavailable) {
		t.Fatalf("ResolveWorkspace(repo with a dangling root symlink) = %v, want ErrWorkspaceUnavailable", err)
	}
}

func TestResolveWorkspace_RewritesAnAbsoluteInRepoSymlink(t *testing.T) {
	// An absolute link names a host path, which is not where the repo
	// lives inside the container - carried verbatim it would dangle.
	dir := newRepoLayout(t)
	if err := os.Symlink(filepath.Join(dir, "go.mod"), filepath.Join(dir, "AGENT.md")); err != nil {
		t.Skipf("cannot create a symlink here, skipping: %v", err)
	}

	workspace, err := ResolveWorkspace(dir, []string{"cmd", "go.mod", "AGENT.md"})
	if err != nil {
		t.Fatalf("ResolveWorkspace(repo with an absolute in-repo root symlink) = %v, want it carried", err)
	}
	defer workspace.Cleanup()

	target, err := os.Readlink(filepath.Join(workspace.Root, "AGENT.md"))
	if err != nil {
		t.Fatalf("the in-repo symlink was not recreated in the workspace: %v", err)
	}
	if target != "go.mod" {
		t.Fatalf("recreated link points at %q, want it rewritten to the repo-relative %q", target, "go.mod")
	}
}

func TestResolveWorkspace_RefusesAPathDockerCannotExpress(t *testing.T) {
	dir := newRepoLayout(t)
	if err := os.WriteFile(filepath.Join(dir, "a:b"), nil, 0o644); err != nil {
		t.Skipf("cannot create a file with a colon in its name here, skipping: %v", err)
	}

	_, err := ResolveWorkspace(dir, []string{"cmd", "go.mod", "a:b"})

	if !errors.Is(err, ErrWorkspaceUnavailable) {
		t.Fatalf("ResolveWorkspace(repo with a colon in a path) = %v, want ErrWorkspaceUnavailable", err)
	}
	if !strings.Contains(err.Error(), "a:b") {
		t.Fatalf("error = %q, want it to name the path", err)
	}
}

func TestResolveWorkspace_RefusesAnUnreadableRepoRoot(t *testing.T) {
	_, err := ResolveWorkspace(filepath.Join(t.TempDir(), "absent"), nil)

	if !errors.Is(err, ErrWorkspaceUnavailable) {
		t.Fatalf("ResolveWorkspace(absent repo root) = %v, want ErrWorkspaceUnavailable", err)
	}
}
