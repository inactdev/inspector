package inspector

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/inactdev/inspector/internal/container"
)

// ErrTermsUnavailable means the folder a caller named with -terms cannot
// be used as this run's terms, so nothing was judged. Callers must refuse
// rather than falling back to the repository's own terms: a run told to
// read its terms from one place and reading them from another judges the
// project against something nobody asked for.
var ErrTermsUnavailable = errors.New("the terms folder could not be used")

// Terms is where one run reads the project's terms from: ConfigFileName,
// ProjectDockerfileName, and whatever check script the first of those
// names.
//
// Ordinarily that is the repository root, and the terms are part of the
// commit under check - committed, reviewable, and protected by name by
// inspector-gate. Issue #69 adds the other case: a repository that may
// not carry anything of inspector's at all, whose terms live in a folder
// beside it named with -terms. Everything that follows from that is the
// same two facts. The terms are not part of the commit, so they cannot be
// required to be committed to it and the gate has nothing there to
// protect; and the check command cannot see them unless they are carried
// into the container, which is what Mount is for.
type Terms struct {
	// Dir is the directory holding them, on the host, as an absolute
	// path with every symbolic link already resolved - it becomes a
	// mount source, and Docker and git alike want the physical path.
	Dir string
	// Outside reports that Dir is not the repository under check.
	Outside bool
}

// ResolveTerms resolves where a run reads its terms from. An empty dir -
// no -terms flag - is the ordinary case and names the repository root
// itself. Anything else is refused by name rather than guessed at, with
// every error wrapping ErrTermsUnavailable.
//
// A folder that holds the repository, or that the repository holds, is
// refused on purpose:
//
//   - Terms *inside* the repository are in-repo terms at a path
//     inspector-gate does not protect, since the gate protects
//     .inspector.json and Dockerfile.inspector by name from the base
//     branch. That is an unprotected copy of the thing that decides
//     whether work passes (SPEC.md section 8).
//   - Terms that *contain* the repository - a parent directory, a home
//     directory - would mount that whole ancestor into the check
//     container. What a check command can reach has to be declared,
//     never inherited from wherever the repository happens to sit.
func ResolveTerms(repoRoot, dir string) (Terms, error) {
	if strings.TrimSpace(dir) == "" {
		return Terms{Dir: repoRoot}, nil
	}

	absolute, err := filepath.Abs(dir)
	if err != nil {
		return Terms{}, unavailableTerms("%q could not be resolved to an absolute path: %v", dir, err)
	}
	// Resolved for the same reason the git directory's mount source and
	// the workspace's root are: this path becomes a mount source, and
	// Docker wants the physical one (macOS's /var -> /private/var).
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return Terms{}, unavailableTerms("%q does not resolve to a real, existing path: %v", dir, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Terms{}, unavailableTerms("%q could not be read: %v", resolved, err)
	}
	if !info.IsDir() {
		return Terms{}, unavailableTerms("%q is not a directory, and -terms names the folder holding %s, %s and this project's check script", resolved, ConfigFileName, ProjectDockerfileName)
	}
	// Docker's own -v syntax separates source, target and options with
	// colons, so a path containing one cannot be expressed in it at all.
	// Named here rather than handed over to be mis-split, exactly as
	// container.ResolveWorkspace does for the repository's own entries.
	if strings.Contains(resolved, ":") {
		return Terms{}, unavailableTerms("%q contains a colon, which Docker's own mount syntax uses as a separator, so inspector cannot mount it into the check container", resolved)
	}

	resolvedRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return Terms{}, unavailableTerms("%q, the repository under check, does not resolve to a real, existing path: %v", repoRoot, err)
	}
	if containsPath(resolvedRepoRoot, resolved) {
		return Terms{}, unavailableTerms("%q is inside the repository under check (%q), so these would be in-repo terms at a path inspector-gate does not protect - it protects %s and %s by name, from the base branch. Terms inside the repository belong at the repository root, with no -terms flag", resolved, resolvedRepoRoot, ConfigFileName, ProjectDockerfileName)
	}
	if containsPath(resolved, resolvedRepoRoot) {
		return Terms{}, unavailableTerms("%q holds the repository under check (%q), and inspector would mount that whole directory into the check container read-only - what a check command can reach has to be declared, not inherited from wherever the repository sits. Put the terms in a folder of their own", resolved, resolvedRepoRoot)
	}
	return Terms{Dir: resolved, Outside: true}, nil
}

// Config reads the project's own definition of green from wherever this
// run's terms live. Its errors are the same ones LoadConfig has always
// returned, so a caller's refusals need no new cases.
func (t Terms) Config() (*Config, error) {
	return LoadConfig(t.Dir)
}

// Dockerfile resolves the ProjectDockerfileName the check image is built
// from. The repository's own terms must have it committed, for the reason
// the working tree has to be clean: an image built from a file that is not
// in the commit cannot be bound to that commit, and nobody reviewing the
// work would see it. A -terms folder is outside every repository inspector
// was handed, so there is nothing for it to be committed to and the file
// only has to be there - which is the loss that comes with the issue's
// requirement, and is stated in README rather than hidden.
func (t Terms) Dockerfile() (string, error) {
	if t.Outside {
		return projectDockerfilePath(t.Dir)
	}
	return ProjectDockerfile(t.Dir)
}

// Mount is what the check container needs so the check command can reach
// a terms folder outside the repository, at container.TermsDir and
// read-only. The zero Mount, for the repository's own terms, mounts
// nothing: they are already in the workspace.
func (t Terms) Mount() container.Mount {
	if !t.Outside {
		return container.Mount{}
	}
	return container.Mount{Source: t.Dir, Target: container.TermsDir}
}

// Location names where these terms are read from, for the refusals that
// tell someone which file to add and where to put it.
func (t Terms) Location() string {
	if t.Outside {
		return fmt.Sprintf("the terms folder %s", t.Dir)
	}
	return "the repo root"
}

// containsPath reports whether child is parent itself or lies under it.
func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func unavailableTerms(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTermsUnavailable, fmt.Sprintf(format, args...))
}
