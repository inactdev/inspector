package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// MaxWorkspaceMounts bounds how many bind mounts one run's workspace
// may carry. The ordinary number is the repo root's own top-level
// entries; it grows past that only for a directory git would ignore a
// new file in, which is laid out entry by entry (issue #84, and see
// SourceEntry). A repository that tracks thousands of files inside such
// a directory is refused by name rather than handed to Docker as a
// command line long enough to fail on its own, or as a container that
// takes minutes to start.
//
// Fixed with no config key, like inspector.MaxOutputBytes: a project
// that can raise the cap can bring back the failure it exists to
// prevent.
const MaxWorkspaceMounts = 1024

// SourceEntry is one entry of the commit under check as git reads it -
// what ResolveWorkspace needs to know about a tracked path beyond its
// name, which the caller reads from git and this package never guesses.
type SourceEntry struct {
	// Name is the entry's own name within the directory holding it,
	// never a path.
	Name string
	// IgnoresNewFiles says, for a directory, that git would ignore a
	// new file placed directly in it - so nothing a check command
	// writes there can ever be part of the commit under check, and
	// issue #84's reason for blocking the write is gone. Such a
	// directory is writable, with every tracked file in it mounted
	// read-only on its own; a directory git would *not* ignore a new
	// file in is mounted read-only as a whole, which is what issue #27
	// did for every tracked directory.
	IgnoresNewFiles bool
	// Entries are the tracked entries directly inside this directory,
	// each carrying its own answer. Meaningful only when
	// IgnoresNewFiles is set: a directory mounted read-only as a whole
	// is read-only for its entire subtree, so what it holds needs no
	// mount of its own.
	Entries []SourceEntry
}

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
	// (plus .git) into that workspace, read-only, at their own names -
	// and, inside a tracked directory git would ignore a new file in,
	// each tracked file in it on its own (issue #84). A bind mount
	// cannot be unlinked or renamed from inside the container, so these
	// paths can be neither written nor replaced by a writable file of
	// the same name.
	SourceMounts []Mount
	// WritableMounts carry the repo root's remaining top-level entries -
	// gitignored build output, installed dependencies, inspector's own
	// report directory - at their own names, still writable, exactly as
	// the whole repo was before issue #27. None of them is tracked
	// source, so none of them is what the commit under check is made
	// of; keeping them writable is what lets a cached node_modules go
	// on being reused. A tracked directory git would ignore a new file
	// in is here too, as the throwaway stand-in writableRoot holds
	// rather than the repository's own directory (issue #84).
	WritableMounts []Mount
	// writableRoot is this run's second throwaway tree: one directory
	// in it per tracked directory git would ignore a new file in, each
	// bind-mounted read-write where that directory belongs in the
	// workspace. Unexported because it is scratch space like Root, and
	// Cleanup discards it the same way. Empty when the repository has
	// no such directory, which is the ordinary case.
	writableRoot string
}

// Cleanup discards this run's writable workspace, including everything
// the check command wrote into it. Safe to call more than once, and on
// a zero Workspace.
func (w Workspace) Cleanup() {
	if w.Root != "" {
		os.RemoveAll(w.Root)
	}
	if w.writableRoot != "" {
		os.RemoveAll(w.writableRoot)
	}
}

// ResolveWorkspace assembles the workspace for one check run against
// repoRoot. trackedTopLevel names the repo root's own top-level entries
// git tracks - the first path component of every tracked file - each
// with git's own answer to whether it would ignore a new file placed
// directly in it, which the caller reads from git; everything else
// found in the repo root is treated as the untracked, writable
// leftovers of previous runs.
//
// Errors wrap ErrWorkspaceUnavailable and name what could not be
// carried into the container. Nothing here guesses: an entry inspector
// cannot mount read-only without changing what the container can reach
// is refused by name rather than mounted writable, dropped, or
// followed.
func ResolveWorkspace(repoRoot string, trackedTopLevel []SourceEntry) (Workspace, error) {
	// Resolved for the symlink test below, which has to compare real
	// paths: a repo reached through a symlinked parent would otherwise
	// make every link in it look like it points somewhere else.
	resolvedRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return Workspace{}, unavailableWorkspace("%q does not resolve to a real, existing path: %v", repoRoot, err)
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

	builder := &workspaceBuilder{repoRoot: resolvedRepoRoot, workspace: Workspace{Root: resolvedRoot}}
	if err := builder.layout(repoRoot, resolvedRoot, WorkspaceDir, "", trackedTopLevel, true); err != nil {
		builder.workspace.Cleanup()
		return Workspace{}, err
	}
	return builder.workspace, nil
}

// workspaceBuilder accumulates one run's workspace as layout walks the
// repository's own directories.
type workspaceBuilder struct {
	// repoRoot is the resolved repository root, for the symlink
	// containment test, which has to compare real paths.
	repoRoot  string
	workspace Workspace
	// mounts counts what has been added to either list, for
	// MaxWorkspaceMounts.
	mounts int
	// standIns names the writable stand-ins apart. See writableStandIn.
	standIns int
}

// layout lays out one directory of the repository under check.
//
// hostDir is that directory on the host, and relative names it within
// the repository ("" for the root). target is where it is inside the
// container, and floor is the host directory in which the mount points
// for its entries are created: the workspace root for the repository
// root, and that directory's own writable stand-in for a directory git
// would ignore a new file in. tracked is what git tracks directly
// inside it. isRoot carries the one thing only the repository root has,
// .git.
func (b *workspaceBuilder) layout(hostDir, floor, target, relative string, tracked []SourceEntry, isRoot bool) error {
	entries, err := os.ReadDir(hostDir)
	if err != nil {
		return unavailableWorkspace("%q could not be read, so inspector cannot tell what the check command would be given: %v", hostDir, err)
	}
	trackedByName := make(map[string]SourceEntry, len(tracked))
	for _, entry := range tracked {
		trackedByName[entry.Name] = entry
	}

	for _, entry := range entries {
		name := entry.Name()
		source := filepath.Join(hostDir, name)
		mountPoint := filepath.Join(floor, name)
		childTarget := target + "/" + name
		childRelative := name
		if relative != "" {
			childRelative = relative + "/" + name
		}
		trackedEntry, isTracked := trackedByName[name]
		if isRoot && name == gitEntryName {
			isTracked = true
			trackedEntry = SourceEntry{Name: name}
		}

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
			text, err := containerLinkTarget(b.repoRoot, source)
			if err != nil {
				return err
			}
			if err := os.Symlink(text, mountPoint); err != nil {
				return unavailableWorkspace("recreating the symbolic link %q in this run's workspace: %v", source, err)
			}
		case entry.IsDir():
			if err := os.Mkdir(mountPoint, 0o755); err != nil {
				return unavailableWorkspace("creating the mount point for %q in this run's workspace: %v", source, err)
			}
			if isTracked && trackedEntry.IgnoresNewFiles {
				// git would ignore a new file placed directly in here,
				// so no such file can be part of the commit under
				// check and issue #27's reason to block the write is
				// gone - a Rails app's tmp/ and log/, which hold one
				// tracked placeholder each and are written into at
				// boot, are the case this exists for (issue #84). What
				// the check writes goes to a throwaway stand-in rather
				// than the repository, and every tracked file in here
				// is still mounted read-only on its own below.
				standIn, err := b.writableStandIn(childRelative)
				if err != nil {
					return err
				}
				if err := b.addMount(standIn, childTarget, false); err != nil {
					return err
				}
				if err := b.layout(source, standIn, childTarget, childRelative, trackedEntry.Entries, false); err != nil {
					return err
				}
				continue
			}
			if err := b.addMount(source, childTarget, isTracked); err != nil {
				return err
			}
		case entry.Type().IsRegular():
			// Docker would create a missing mount point itself, but
			// inside the workspace mount - so creating them here keeps
			// the type and ownership inspector's own rather than
			// whatever the daemon decides.
			if err := os.WriteFile(mountPoint, nil, 0o644); err != nil {
				return unavailableWorkspace("creating the mount point for %q in this run's workspace: %v", source, err)
			}
			if err := b.addMount(source, childTarget, isTracked); err != nil {
				return err
			}
		default:
			return unavailableWorkspace("%q is neither a directory nor a regular file (%s), and inspector mounts this directory's entries one by one - it has nothing to mount this as", source, entry.Type())
		}
	}
	return nil
}

// addMount records one bind mount, read-only for tracked content and
// writable for everything else, and refuses rather than let a run grow
// past MaxWorkspaceMounts.
func (b *workspaceBuilder) addMount(source, target string, readOnly bool) error {
	// Docker's own -v syntax separates source, target and options with
	// colons, so a path containing one cannot be expressed in it at
	// all. Named rather than handed over to be mis-split.
	for _, path := range []string{source, target} {
		if strings.Contains(path, ":") {
			return unavailableWorkspace("%q contains a colon, which Docker's own mount syntax uses as a separator, so inspector cannot mount it", path)
		}
	}
	if b.mounts >= MaxWorkspaceMounts {
		return unavailableWorkspace("this repository needs more than %d separate bind mounts to reach the check container, and %q is where it crossed that line - a directory git would ignore a new file in is carried entry by entry, so a tracked tree of that size inside one needs either fewer tracked files in it or an ignore rule that does not cover it", MaxWorkspaceMounts, source)
	}
	b.mounts++

	mount := Mount{Source: source, Target: target}
	if readOnly {
		b.workspace.SourceMounts = append(b.workspace.SourceMounts, mount)
	} else {
		b.workspace.WritableMounts = append(b.workspace.WritableMounts, mount)
	}
	return nil
}

// writableStandIn returns the host directory that stands in for the
// repository directory at relative - a fresh, empty directory in this
// run's second throwaway tree, created on first use and discarded with
// the workspace.
//
// It is mounted rather than simply created inside the workspace, which
// would be one mount cheaper. A bind-mounted *file* cannot be unlinked
// or renamed over from inside the container - the fact the sanitized
// git config already rides on - but a plain directory holding those
// file mounts can itself be renamed, and the mounts travel with it. A
// check could then move a tracked directory aside, rebuild it, and pass
// against files the commit does not contain: issue #27's laundering,
// arriving by a different door. A mounted directory refuses the rename
// with EBUSY, which is how WorkspaceDir itself already resists it.
//
// The stand-ins are numbered rather than laid out mirroring the
// repository's own paths, so that no stand-in ever sits inside another
// one: a nested writable directory's mount point has to be created
// inside its parent's stand-in, and a mirrored layout would make that
// mount point and the stand-in the same host path - a directory
// mounted over itself, which is a stranger thing to ask Docker for
// than two plainly separate paths.
func (b *workspaceBuilder) writableStandIn(relative string) (string, error) {
	if b.workspace.writableRoot == "" {
		root, err := os.MkdirTemp("", "inspector-writable-")
		if err != nil {
			return "", unavailableWorkspace("creating the writable stand-in tree for the directories git would ignore a new file in: %v", err)
		}
		// Resolved for the same reason the workspace root is: it
		// becomes a mount source.
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			os.RemoveAll(root)
			return "", unavailableWorkspace("resolving %q, this run's writable stand-in tree: %v", root, err)
		}
		b.workspace.writableRoot = resolved
	}
	b.standIns++
	standIn := filepath.Join(b.workspace.writableRoot, strconv.Itoa(b.standIns))
	if err := os.Mkdir(standIn, 0o755); err != nil {
		return "", unavailableWorkspace("creating %q, the writable stand-in for %q, a directory git would ignore a new file in: %v", standIn, relative, err)
	}
	return standIn, nil
}

// containerLinkTarget returns what a symlink in the repository has to
// point at inside the container for it to mean there what it means on
// the host, and refuses the link when it leads out of the repository.
//
// The link's own text is kept whenever it is relative and cannot climb
// out of the repo root from where the link itself sits, because that is
// the form git recorded and the one a check running `git status` has to
// see unchanged. An absolute or climbing text names a host path that is
// not where the repo lives inside the container, so it is replaced by
// the equivalent path relative to the link's own directory.
func containerLinkTarget(resolvedRepoRoot, source string) (string, error) {
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
	// Where the link itself sits, which is what both its own text and
	// the rewritten form below are relative to - the repo root for a
	// top-level link, a directory inside it for a link in a directory
	// git would ignore a new file in (issue #84).
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(source))
	if err != nil {
		return "", unavailableWorkspace("%q is a symbolic link whose own directory does not resolve to a real, existing path: %v", source, err)
	}
	relativeDir, err := filepath.Rel(resolvedRepoRoot, resolvedDir)
	if err != nil {
		return "", unavailableWorkspace("%q is a symbolic link inside %q, which inspector cannot place within the repository: %v", source, resolvedDir, err)
	}
	if !filepath.IsAbs(text) && !escapesRepo(filepath.Clean(filepath.Join(relativeDir, text))) {
		return text, nil
	}
	rewritten, err := filepath.Rel(resolvedDir, resolvedTarget)
	if err != nil {
		return "", unavailableWorkspace("%q is a symbolic link naming %q, which inspector cannot express relative to the link's own directory: %v", source, resolvedTarget, err)
	}
	return filepath.ToSlash(rewritten), nil
}

// escapesRepo reports whether a cleaned relative path leads out of the
// directory it is relative to.
func escapesRepo(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || strings.HasPrefix(relative, "../")
}

func unavailableWorkspace(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrWorkspaceUnavailable, fmt.Sprintf(format, args...))
}
