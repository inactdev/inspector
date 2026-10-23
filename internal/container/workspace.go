package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrWorkspaceUnavailable means the check command's workspace - the
// commit under check, read-only, plus a writable area to work in -
// could not be assembled, so the check never ran. Callers must refuse
// the run rather than report that failure as a verdict, and must never
// fall back to a writable repo: a check that can write the code it is
// being judged on can pass against bytes that exist nowhere (issue
// #27), which is the one thing this exists to prevent.
var ErrWorkspaceUnavailable = errors.New("the check command's workspace could not be assembled")

// gitEntryName is the repo root entry git owns. It is never tracked, and
// it is read-only alongside tracked source for the same reason: it is
// the commit under check, and a check command that can rewrite refs or
// objects can change what it is being judged on.
const gitEntryName = ".git"

// Workspace is what one containerized check command sees at
// WorkspaceDir: a throwaway writable directory with the commit under
// check mounted into it read-only, one mount per top-level entry.
//
// Why not simply mount the repo read-only: an ordinary check writes
// into its own tree - installed packages, compiled output, coverage and
// test reports - and the issue's bar is that such a project passes
// unchanged, with no configuration. A read-only repo mount fails that
// at the first `mkdir dist`. Mounting a writable directory and laying
// the repo's own entries over it read-only gives both: a writable
// repo root, and tracked paths that cannot be written through.
//
// Why not an overlay (lowerdir the repo, upperdir a scratch area),
// which is the other way to get a writable repo root: overlayfs copies
// a lower file up on first write, so a check could modify a tracked
// file in the upper layer, pass against those bytes, and have them
// vanish with the discarded upper layer - exactly the laundering issue
// #27 closes, and less detectable than the writable mount it replaced.
// There is no overlay mode that denies copy-up of existing files while
// allowing new ones. See notes/20261023-source-read-only-during-checks.md.
type Workspace struct {
	// Root is the host directory bind-mounted read-write at
	// WorkspaceDir. It belongs to one run: Cleanup discards it, and
	// nothing written there reaches the repository.
	Root string
	// SourceMounts carry the repo root's own tracked top-level entries
	// (plus .git) into that workspace, read-only, at their own names.
	// A bind mount cannot be unlinked or renamed from inside the
	// container, so these paths can be neither written nor replaced by
	// a writable file of the same name.
	SourceMounts []Mount
	// WritableMounts carry the repo root's remaining top-level entries -
	// gitignored build output, installed dependencies, inspector's own
	// report directory - at their own names, still writable, exactly as
	// the whole repo was before issue #27. None of them is tracked
	// source, so none of them is what the commit under check is made
	// of; keeping them writable is what lets a cached node_modules go
	// on being reused.
	WritableMounts []Mount
}

// Cleanup discards this run's writable workspace, including everything
// the check command wrote into it. Safe to call more than once, and on
// a zero Workspace.
func (w Workspace) Cleanup() {
	if w.Root != "" {
		os.RemoveAll(w.Root)
	}
}

// ResolveWorkspace assembles the workspace for one check run against
// repoRoot. trackedTopLevel names the repo root's own top-level entries
// git tracks - the first path component of every tracked file - which
// the caller reads from git; everything else found in the repo root is
// treated as the untracked, writable leftovers of previous runs.
//
// Errors wrap ErrWorkspaceUnavailable and name what could not be
// carried into the container. Nothing here guesses: an entry inspector
// cannot mount read-only without changing what the container can reach
// is refused by name rather than mounted writable, dropped, or
// followed.
func ResolveWorkspace(repoRoot string, trackedTopLevel []string) (Workspace, error) {
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		return Workspace{}, unavailableWorkspace("%q could not be read, so inspector cannot tell what the check command would be given: %v", repoRoot, err)
	}
	// Resolved for the symlink test below, which has to compare real
	// paths: a repo reached through a symlinked parent would otherwise
	// make every link in it look like it points somewhere else.
	resolvedRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return Workspace{}, unavailableWorkspace("%q does not resolve to a real, existing path: %v", repoRoot, err)
	}
	tracked := make(map[string]bool, len(trackedTopLevel))
	for _, name := range trackedTopLevel {
		tracked[name] = true
	}

	root, err := os.MkdirTemp("", "inspector-workspace-")
	if err != nil {
		return Workspace{}, unavailableWorkspace("creating the writable workspace this run's check command works in: %v", err)
	}
	// Resolved for the same reason the sanitized git config's directory
	// is: this path becomes a mount source, and git and Docker alike
	// want the physical one (macOS's /var -> /private/var).
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		os.RemoveAll(root)
		return Workspace{}, unavailableWorkspace("resolving %q, the writable workspace this run's check command works in: %v", root, err)
	}
	workspace := Workspace{Root: resolvedRoot}

	for _, entry := range entries {
		name := entry.Name()
		source := filepath.Join(repoRoot, name)
		readOnly := tracked[name] || name == gitEntryName

		switch {
		case entry.Type()&os.ModeSymlink != 0:
			// A symlink cannot be mounted: Docker resolves the source
			// path, so mounting one means mounting whatever it points
			// at. For a target outside the repository that would put a
			// host path the check command has no business reading
			// inside the container, which is the containment bar issue
			// #13 set, so those are refused by name. For a target
			// inside it the resolved form would be worse than useless:
			// a linked directory would arrive as a second real copy of
			// a tree the commit has once, changing what a build walks.
			// So an in-repo link is recreated as a link instead, which
			// is faithful but leaves that one path writable - the
			// residual issue #27 cannot close with a mount flag, and
			// README states it.
			target, err := repoRelativeLinkTarget(resolvedRepoRoot, source)
			if err != nil {
				workspace.Cleanup()
				return Workspace{}, err
			}
			if err := os.Symlink(target, filepath.Join(resolvedRoot, name)); err != nil {
				workspace.Cleanup()
				return Workspace{}, unavailableWorkspace("recreating the symbolic link %q in this run's workspace: %v", source, err)
			}
			continue
		case entry.IsDir():
			if err := os.Mkdir(filepath.Join(resolvedRoot, name), 0o755); err != nil {
				workspace.Cleanup()
				return Workspace{}, unavailableWorkspace("creating the mount point for %q in this run's workspace: %v", source, err)
			}
		case entry.Type().IsRegular():
			// Docker would create a missing mount point itself, but
			// inside the workspace mount - so creating them here keeps
			// the type and ownership inspector's own rather than
			// whatever the daemon decides.
			if err := os.WriteFile(filepath.Join(resolvedRoot, name), nil, 0o644); err != nil {
				workspace.Cleanup()
				return Workspace{}, unavailableWorkspace("creating the mount point for %q in this run's workspace: %v", source, err)
			}
		default:
			workspace.Cleanup()
			return Workspace{}, unavailableWorkspace("%q is neither a directory nor a regular file (%s), and inspector mounts the repository root's entries one by one - it has nothing to mount this as", source, entry.Type())
		}

		// Docker's own -v syntax separates source, target and options
		// with colons, so a path containing one cannot be expressed in
		// it at all. Named rather than handed over to be mis-split.
		if strings.Contains(source, ":") {
			workspace.Cleanup()
			return Workspace{}, unavailableWorkspace("%q contains a colon, which Docker's own mount syntax uses as a separator, so inspector cannot mount it", source)
		}

		mount := Mount{Source: source, Target: WorkspaceDir + "/" + name}
		if readOnly {
			workspace.SourceMounts = append(workspace.SourceMounts, mount)
		} else {
			workspace.WritableMounts = append(workspace.WritableMounts, mount)
		}
	}
	return workspace, nil
}

// repoRelativeLinkTarget returns what a repo-root symlink has to point
// at inside the container for it to mean there what it means on the
// host, and refuses the link when it leads out of the repository.
//
// The link's own text is kept whenever it is relative and cannot climb
// out of the repo root on its own, because that is the form git
// recorded and the one a check running `git status` has to see
// unchanged. An absolute or climbing text names a host path that is
// not where the repo lives inside the container, so it is replaced by
// the equivalent path relative to the root.
func repoRelativeLinkTarget(resolvedRepoRoot, source string) (string, error) {
	text, err := os.Readlink(source)
	if err != nil {
		return "", unavailableWorkspace("%q is a symbolic link whose target could not be read: %v", source, err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", unavailableWorkspace("%q is a symbolic link naming %q, which does not resolve to a real, existing path (%v) - inspector carries a link into the container only when it can confirm where it leads", source, text, err)
	}
	relative, err := filepath.Rel(resolvedRepoRoot, resolvedTarget)
	if err != nil || escapesRepo(relative) {
		return "", unavailableWorkspace("%q is a symbolic link leading to %q, outside the repository: inspector would have to mount a host path the commit under check does not contain into the container to honor it, and refuses to", source, resolvedTarget)
	}
	if !filepath.IsAbs(text) && !escapesRepo(filepath.Clean(text)) {
		return text, nil
	}
	return filepath.ToSlash(relative), nil
}

// escapesRepo reports whether a cleaned relative path leads out of the
// directory it is relative to.
func escapesRepo(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || strings.HasPrefix(relative, "../")
}

func unavailableWorkspace(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrWorkspaceUnavailable, fmt.Sprintf(format, args...))
}
