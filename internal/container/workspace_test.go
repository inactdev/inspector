package container

import (
	"errors"
	"fmt"
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

// trackedEntries is what the caller's git reports for the ordinary
// tracked entry: git would not ignore a new file placed in it, so it is
// mounted read-only as a whole (issue #27's layout, unchanged). The
// tests of the other answer build their own SourceEntry values.
func trackedEntries(names ...string) []SourceEntry {
	entries := make([]SourceEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, SourceEntry{Name: name})
	}
	return entries
}

// mountsByTarget indexes one mount list by where it lands in the
// container, which is how every assertion below names a mount.
func mountsByTarget(mounts []Mount) map[string]string {
	byTarget := make(map[string]string, len(mounts))
	for _, mount := range mounts {
		byTarget[mount.Target] = mount.Source
	}
	return byTarget
}

func TestResolveWorkspace_TrackedSourceIsReadOnlyAndTheRestIsNot(t *testing.T) {
	dir := newRepoLayout(t)

	workspace, err := ResolveWorkspace(dir, trackedEntries("cmd", "go.mod"))
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

	workspace, err := ResolveWorkspace(dir, trackedEntries("cmd", "go.mod"))
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

	workspace, err := ResolveWorkspace(dir, trackedEntries("cmd", "go.mod"))
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

	workspace, err := ResolveWorkspace(dir, trackedEntries("cmd", "go.mod", "AGENT.md"))
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

	workspace, err := ResolveWorkspace(dir, trackedEntries("cmd", "go.mod", "config"))
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

	_, err := ResolveWorkspace(dir, trackedEntries("cmd", "go.mod", "config"))

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

	workspace, err := ResolveWorkspace(dir, trackedEntries("cmd", "go.mod", "AGENT.md"))
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

	_, err := ResolveWorkspace(dir, trackedEntries("cmd", "go.mod", "a:b"))

	if !errors.Is(err, ErrWorkspaceUnavailable) {
		t.Fatalf("ResolveWorkspace(repo with a colon in a path) = %v, want ErrWorkspaceUnavailable", err)
	}
	if !strings.Contains(err.Error(), "a:b") {
		t.Fatalf("error = %q, want it to name the path", err)
	}
}

// newIgnoringLayout builds the shape issue #84 exists for: a Rails-like
// repo root tracking one placeholder in each of two directories it
// writes into at boot, a cached gitignored leftover inside one of them,
// and ordinary source beside them. The SourceEntry tree returned is
// what the caller's git reports about it - tmp/ and tmp/pids/ and log/
// ignore a new file, src/ does not.
func newIgnoringLayout(t *testing.T) (string, []SourceEntry) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	for _, sub := range []string{".git", "log", "src", "tmp/pids", "tmp/cache"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(sub)), 0o755); err != nil {
			t.Fatalf("creating %s: %v", sub, err)
		}
	}
	for _, file := range []string{"log/.keep", "src/main.rb", "tmp/.keep", "tmp/pids/.keep", "tmp/cache/cached"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(file)), []byte(file+"\n"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", file, err)
		}
	}
	return dir, []SourceEntry{
		{Name: "log", IgnoresNewFiles: true, Entries: trackedEntries(".keep")},
		{Name: "src"},
		{Name: "tmp", IgnoresNewFiles: true, Entries: []SourceEntry{
			{Name: ".keep"},
			{Name: "pids", IgnoresNewFiles: true, Entries: trackedEntries(".keep")},
		}},
	}
}

func TestResolveWorkspace_ADirectoryGitWouldIgnoreANewFileInIsWritable(t *testing.T) {
	// Issue #84: a directory holding one tracked placeholder was
	// unwritable in full, so a Rails suite died at Errno::EROFS on
	// tmp/local_secret.txt before a test ran. Nothing git ignores can
	// be part of the commit under check, so blocking those writes
	// bought nothing - but every tracked file in there is still
	// read-only, one mount each.
	dir, tracked := newIgnoringLayout(t)

	workspace, err := ResolveWorkspace(dir, tracked)
	if err != nil {
		t.Fatalf("ResolveWorkspace(%q) = %v, want the mounts a check command runs against", dir, err)
	}
	defer workspace.Cleanup()

	readOnly := mountsByTarget(workspace.SourceMounts)
	writable := mountsByTarget(workspace.WritableMounts)

	for _, name := range []string{"tmp/.keep", "tmp/pids/.keep", "log/.keep"} {
		target := WorkspaceDir + "/" + name
		if readOnly[target] != filepath.Join(dir, filepath.FromSlash(name)) {
			t.Fatalf("%s is not mounted read-only from the repository: SourceMounts = %v", name, workspace.SourceMounts)
		}
		if _, ok := writable[target]; ok {
			t.Fatalf("%s is mounted writable as well as read-only: WritableMounts = %v", name, workspace.WritableMounts)
		}
	}

	// The directories themselves are writable - and mounted, not
	// merely created in the workspace: a plain directory holding those
	// file mounts could be renamed out of the way and rebuilt, which
	// would let a check pass against a tracked file the commit does
	// not contain.
	for _, name := range []string{"tmp", "tmp/pids", "log"} {
		target := WorkspaceDir + "/" + name
		standIn, ok := writable[target]
		if !ok {
			t.Fatalf("%s is not mounted writable, so the check command cannot write in it: WritableMounts = %v", name, workspace.WritableMounts)
		}
		if standIn == filepath.Join(dir, filepath.FromSlash(name)) {
			t.Fatalf("%s is mounted writable from the repository itself (%q), want a throwaway stand-in", name, standIn)
		}
		if info, err := os.Stat(standIn); err != nil || !info.IsDir() {
			t.Fatalf("os.Stat(%q), the stand-in for %s = %v, want a directory", standIn, name, err)
		}
		if _, ok := readOnly[target]; ok {
			t.Fatalf("%s is mounted read-only as well as writable: SourceMounts = %v", name, workspace.SourceMounts)
		}
	}

	// A directory git would not ignore a new file in is untouched by
	// all of this: one read-only mount for the whole subtree, exactly
	// as issue #27 left it, and no mount of its own for what is inside.
	if readOnly[WorkspaceDir+"/src"] != filepath.Join(dir, "src") {
		t.Fatalf("src is not mounted read-only as a whole: SourceMounts = %v", workspace.SourceMounts)
	}
	for _, target := range []string{WorkspaceDir + "/src/main.rb"} {
		if _, ok := readOnly[target]; ok {
			t.Fatalf("%s got a mount of its own; a read-only directory mount already covers its whole subtree", target)
		}
	}
}

func TestResolveWorkspace_WritableDirectoryCarriesMountPointsAndItsGitignoredLeftovers(t *testing.T) {
	dir, tracked := newIgnoringLayout(t)

	workspace, err := ResolveWorkspace(dir, tracked)
	if err != nil {
		t.Fatalf("ResolveWorkspace(%q) = %v, want the mounts a check command runs against", dir, err)
	}
	defer workspace.Cleanup()

	writable := mountsByTarget(workspace.WritableMounts)
	// A gitignored leftover inside a writable tracked directory keeps
	// being mounted from the host, writable, for the same reason a
	// cached node_modules at the repo root does: it is not what the
	// commit under check is made of, and reusing it is the point.
	if writable[WorkspaceDir+"/tmp/cache"] != filepath.Join(dir, "tmp", "cache") {
		t.Fatalf("tmp/cache is not mounted writable from the repository: WritableMounts = %v", workspace.WritableMounts)
	}

	// Docker needs a mount point of the matching kind at every
	// destination, and for a writable directory's entries that lands in
	// its stand-in rather than in the workspace root.
	standIn := writable[WorkspaceDir+"/tmp"]
	for name, wantDir := range map[string]bool{".keep": false, "pids": true, "cache": true} {
		info, err := os.Lstat(filepath.Join(standIn, name))
		if err != nil {
			t.Fatalf("no mount point for tmp/%s in the stand-in for tmp: %v", name, err)
		}
		if info.IsDir() != wantDir {
			t.Fatalf("mount point for tmp/%s is a directory = %v, want %v", name, info.IsDir(), wantDir)
		}
	}
	if err := os.WriteFile(filepath.Join(standIn, "local_secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatalf("the stand-in for tmp is not writable: %v", err)
	}
	// Nothing written there reaches the repository, exactly as for the
	// workspace root.
	if _, err := os.Stat(filepath.Join(dir, "tmp", "local_secret.txt")); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(tmp/local_secret.txt) in the repository = %v, want the write kept out of it", err)
	}
}

func TestResolveWorkspace_CleanupDiscardsTheWritableStandIns(t *testing.T) {
	dir, tracked := newIgnoringLayout(t)

	workspace, err := ResolveWorkspace(dir, tracked)
	if err != nil {
		t.Fatalf("ResolveWorkspace(%q) = %v, want the mounts a check command runs against", dir, err)
	}
	standIn := mountsByTarget(workspace.WritableMounts)[WorkspaceDir+"/tmp"]
	if standIn == "" {
		t.Fatal("tmp is not mounted writable, so this test proves nothing")
	}

	workspace.Cleanup()

	if _, err := os.Stat(standIn); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%q) = %v after Cleanup, want the stand-in gone with the rest of the run's writable space", standIn, err)
	}
}

func TestResolveWorkspace_CarriesALinkInAWritableDirectoryAsALink(t *testing.T) {
	// The repo-root residual applies one level down as well, and the
	// link text has to be read from where the link itself sits: a
	// relative text git recorded is kept, an absolute one names a host
	// path that is nowhere inside the container and is rewritten
	// relative to the link's own directory.
	dir, tracked := newIgnoringLayout(t)
	if err := os.Symlink("../src/main.rb", filepath.Join(dir, "tmp", "relative.rb")); err != nil {
		t.Skipf("cannot create a symlink here, skipping: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "src", "main.rb"), filepath.Join(dir, "tmp", "absolute.rb")); err != nil {
		t.Skipf("cannot create a symlink here, skipping: %v", err)
	}
	for i, entry := range tracked {
		if entry.Name == "tmp" {
			tracked[i].Entries = append(entry.Entries, trackedEntries("relative.rb", "absolute.rb")...)
		}
	}

	workspace, err := ResolveWorkspace(dir, tracked)
	if err != nil {
		t.Fatalf("ResolveWorkspace(repo with links in a writable tracked directory) = %v, want them carried", err)
	}
	defer workspace.Cleanup()

	standIn := mountsByTarget(workspace.WritableMounts)[WorkspaceDir+"/tmp"]
	for name, want := range map[string]string{"relative.rb": "../src/main.rb", "absolute.rb": "../src/main.rb"} {
		target, err := os.Readlink(filepath.Join(standIn, name))
		if err != nil {
			t.Fatalf("the link tmp/%s was not recreated in the workspace: %v", name, err)
		}
		if target != want {
			t.Fatalf("recreated link tmp/%s points at %q, want %q - where it leads from its own directory", name, target, want)
		}
	}
}

func TestResolveWorkspace_RefusesMoreMountsThanItCanCarry(t *testing.T) {
	// A directory git would ignore a new file in is carried entry by
	// entry, so a repository tracking thousands of files in one would
	// otherwise hand Docker a command line long enough to fail on its
	// own. Refused by name instead, with the path that crossed the
	// line.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil {
		t.Fatalf("creating tmp: %v", err)
	}
	names := make([]string, 0, MaxWorkspaceMounts+2)
	for i := 0; i <= MaxWorkspaceMounts+1; i++ {
		name := fmt.Sprintf("tracked-%04d", i)
		if err := os.WriteFile(filepath.Join(dir, "tmp", name), nil, 0o644); err != nil {
			t.Fatalf("writing tmp/%s: %v", name, err)
		}
		names = append(names, name)
	}

	workspace, err := ResolveWorkspace(dir, []SourceEntry{{Name: "tmp", IgnoresNewFiles: true, Entries: trackedEntries(names...)}})
	defer workspace.Cleanup()

	if !errors.Is(err, ErrWorkspaceUnavailable) {
		t.Fatalf("ResolveWorkspace(repo needing more than %d mounts) = %v, want ErrWorkspaceUnavailable", MaxWorkspaceMounts, err)
	}
	if !strings.Contains(err.Error(), "tracked-") {
		t.Fatalf("error = %q, want it to name the path that crossed the line", err)
	}
	if workspace.Root != "" {
		t.Fatalf("a refused ResolveWorkspace returned workspace root %q, want nothing left behind", workspace.Root)
	}
}

func TestResolveWorkspace_RefusesAnUnreadableRepoRoot(t *testing.T) {
	_, err := ResolveWorkspace(filepath.Join(t.TempDir(), "absent"), nil)

	if !errors.Is(err, ErrWorkspaceUnavailable) {
		t.Fatalf("ResolveWorkspace(absent repo root) = %v, want ErrWorkspaceUnavailable", err)
	}
}
