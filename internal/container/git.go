package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrGitDirUnavailable means the repo root's git directory cannot be
// made available inside the container, so a check command that uses git
// would fail in there for a reason that has nothing to do with the code
// under inspection. Callers must refuse the run rather than report that
// failure as a verdict - a wrong red is the one outcome this project
// treats as worse than no verdict (issue #31). It is a pure marker for
// errors.Is - its own wording never reaches anyone, because every
// failure below carries its own reason.
var ErrGitDirUnavailable = errors.New("the repository's git directory could not be made available inside the container")

// linkedWorktree opens the refusals raised once the repo root is known
// to be a linked worktree, so the person reading one is told what this
// repository is before being told what went wrong with it.
const linkedWorktree = "this is a linked git worktree and its git directory is outside the mount"

// gitDirError is one reason the git directory could not be made
// available. It matches ErrGitDirUnavailable without inheriting its
// wording, so a refusal reads as one sentence rather than two.
type gitDirError struct{ reason string }

func (e *gitDirError) Error() string { return e.reason }

func (e *gitDirError) Is(target error) bool { return target == ErrGitDirUnavailable }

func unavailable(format string, args ...any) error {
	return &gitDirError{reason: fmt.Sprintf(format, args...)}
}

// Mount is one host path the container can also see, at a fixed
// absolute path inside it. Every Mount is read-only: the repo root is
// the only writable thing in the container, and the one place where
// anything mounted alongside it would otherwise be modifiable by the
// check command.
type Mount struct {
	Source string
	Target string
}

// GitMounts is what one run needs mounted for git to work inside the
// container, plus the throwaway file that made that safe to do.
type GitMounts struct {
	Mounts []Mount
	// tempDir holds the sanitized config. Callers must Cleanup.
	tempDir string
}

// Cleanup removes the throwaway sanitized config written for this run.
// Safe to call more than once, and on a zero GitMounts.
func (g GitMounts) Cleanup() {
	if g.tempDir != "" {
		os.RemoveAll(g.tempDir)
	}
}

// ResolveGitMounts works out what a repo root needs mounted alongside
// itself for git to work inside the container.
//
// An ordinary clone needs nothing: its .git is a directory inside the
// repo root, which is already the one thing mounted. A linked worktree
// is the case this exists for - `git worktree add` leaves its .git as a
// one-line pointer file naming the project's shared git directory by
// absolute host path, a path outside the repo root entirely, so git
// inside the container follows that pointer to nothing and every git
// command a check runs fails (issue #31; verified: `fatal: not a git
// repository`).
//
// What it returns for that case is the project's real shared git
// directory, to be mounted read-only at its own absolute path so the
// pointer resolves, plus a sanitized throwaway copy of that directory's
// own config to shadow-mount over the real one. The real config is the
// one file in a git directory that can carry a credential, and a check
// command runs arbitrary project code: the config it can read must
// never be the real one. Both halves were established by running them,
// here and in Fabrica's src/line/worktree-git.ts, which this follows:
// dropping config entirely breaks git's repository detection outright
// (`fatal: not a git repository: (null)`), while a [core]-only config
// leaves status, log and diff working and `git config --get
// remote.origin.url` returning nothing.
//
// Copying the object database into a standalone git directory per run
// was the alternative, and is deliberately not what this does: it was
// built and rejected in Fabrica as slow and disk-heavy, and it would
// need a sync-back for anything written inside.
func ResolveGitMounts(repoRoot string) (GitMounts, error) {
	gitPath := filepath.Join(repoRoot, ".git")
	info, err := os.Stat(gitPath)
	if err != nil || info.IsDir() {
		// Either the git directory is already inside the one mount
		// that was always there, or there is no git here at all (as
		// the unit tests of a bare check command have it) - which the
		// mounts cannot fix either way.
		return GitMounts{}, nil
	}

	commonGitDir, err := resolveCommonGitDir(repoRoot)
	if err != nil {
		return GitMounts{}, err
	}
	sanitizedConfig, tempDir, err := writeSanitizedGitConfig(commonGitDir)
	if err != nil {
		return GitMounts{}, err
	}
	return GitMounts{
		Mounts: []Mount{
			{Source: commonGitDir, Target: commonGitDir},
			// Docker layers a file mount over an already-mounted
			// directory's sub-path correctly - verified live here and
			// in Fabrica.
			{Source: sanitizedConfig, Target: filepath.Join(commonGitDir, "config")},
		},
		tempDir: tempDir,
	}, nil
}

// resolveCommonGitDir follows a linked worktree's .git pointer file to
// the project's shared git directory - the object database, refs and
// config every worktree of that project points back to - and returns it
// with every symlink resolved. The resolved form is what the mount
// needs: git records the physical path in the pointer file, so a mount
// at an unresolved path (macOS's /var -> /private/var is the trap)
// leaves git looking somewhere nothing is mounted.
func resolveCommonGitDir(repoRoot string) (string, error) {
	pointerPath := filepath.Join(repoRoot, ".git")
	contents, err := os.ReadFile(pointerPath)
	if err != nil {
		return "", unavailable("%q could not be read, so inspector cannot tell where this repository's git directory is: %v", pointerPath, err)
	}
	pointer := strings.TrimSpace(string(contents))
	target, ok := strings.CutPrefix(pointer, "gitdir:")
	if !ok {
		return "", unavailable("%q is neither a git directory nor a worktree pointer file (expected \"gitdir: <path>\", found %q)", pointerPath, pointer)
	}
	// git 2.48+ can write a relative gitdir here
	// (worktree.useRelativePaths / extensions.relativeWorktrees),
	// resolved against the pointer file's own directory - never the
	// process's working directory.
	worktreeGitDir := strings.TrimSpace(target)
	if !filepath.IsAbs(worktreeGitDir) {
		worktreeGitDir = filepath.Join(repoRoot, worktreeGitDir)
	}

	commonPath := filepath.Join(worktreeGitDir, "commondir")
	contents, err = os.ReadFile(commonPath)
	if err != nil {
		return "", unavailable("%q points at %q, and no commondir file could be read from it (%v) - a linked git worktree's git directory has one and a submodule's does not; either way that directory is outside the container's mount",
			pointerPath, worktreeGitDir, err)
	}
	commonGitDir := strings.TrimSpace(string(contents))
	if !filepath.IsAbs(commonGitDir) {
		commonGitDir = filepath.Join(worktreeGitDir, commonGitDir)
	}

	resolved, err := filepath.EvalSymlinks(commonGitDir)
	if err != nil {
		return "", unavailable("%s, and %q (the shared git directory its pointer names) does not resolve to a real, existing path: %v",
			linkedWorktree, commonGitDir, err)
	}
	return resolved, nil
}

// forwardedExtensionKeys are the [extensions] keys of the installed git
// that are credential-free by construction, and so the only ones the
// sanitizer copies forward. Extension keys cannot simply be dropped
// like every other non-[core] section: they are structural (hash
// algorithm, ref storage backend, relative worktree paths), so a git
// that cannot see them misreads the repository outright.
// worktreeConfig is deliberately absent - forwarding it would make git
// honor an unsanitized config.worktree file inside the mount, reopening
// the exact channel this sanitizer closes.
var forwardedExtensionKeys = map[string]bool{
	"compatobjectformat":  true,
	"noop":                true,
	"noop-v1":             true,
	"objectformat":        true,
	"partialclone":        true,
	"preciousobjects":     true,
	"refstorage":          true,
	"relativeworktrees":   true,
	"submodulepathconfig": true,
}

var (
	sectionHeader         = regexp.MustCompile(`^\s*\[\s*([^\s\]"]+)`)
	plainExtensionsHeader = regexp.MustCompile(`(?i)^\s*\[\s*extensions\s*\]`)
	configEntry           = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*)\s*(?:=\s*(.*))?$`)
)

// writeSanitizedGitConfig writes a throwaway copy of commonGitDir's own
// config, carrying forward only its [core] section and individually
// allowlisted [extensions] keys, and returns the copy's path along with
// the temporary directory holding it.
//
// An allowlist, not a denylist, because a denylist fails open on the
// first form nobody anticipated - and a git config carries a credential
// in more ways than one: a remote URL with an embedded credential
// (quoted [remote "origin"] or git's deprecated dotted [remote.origin]
// form), an http.<url>.extraheader authorization line, a
// url.<base>.insteadOf rewrite, a [credential] helper, or an
// [include]/[includeIf] pulling any of those in. Everything not
// explicitly forwarded is invisible by construction instead.
func writeSanitizedGitConfig(commonGitDir string) (configPath, tempDir string, err error) {
	realPath := filepath.Join(commonGitDir, "config")
	contents, err := os.ReadFile(realPath)
	if err != nil {
		return "", "", unavailable("%s, and %q could not be read: %v", linkedWorktree, realPath, err)
	}
	sanitized, err := sanitizeGitConfig(string(contents), realPath)
	if err != nil {
		return "", "", err
	}

	dir, err := os.MkdirTemp("", "inspector-sanitized-git-config-")
	if err != nil {
		return "", "", unavailable("creating a directory for the sanitized stand-in for %q: %v", realPath, err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		os.RemoveAll(dir)
		return "", "", unavailable("resolving %q, the directory holding the sanitized stand-in for %q: %v", dir, realPath, err)
	}
	// 0o644 on purpose: the file is bind-mounted into a container whose
	// image chooses its own user, which is rarely the host user that
	// wrote this, and the content is a config with nothing in it worth
	// keeping from anyone.
	path := filepath.Join(resolvedDir, "config")
	if err := os.WriteFile(path, []byte(sanitized), 0o644); err != nil {
		os.RemoveAll(dir)
		return "", "", unavailable("writing the sanitized stand-in for %q: %v", realPath, err)
	}
	return path, resolvedDir, nil
}

// sanitizeGitConfig returns what of config may be forwarded into the
// container. configPath names the real file in any error, which is
// always a refusal rather than a guess: an [extensions] key it cannot
// vouch for is neither dropped (which would misread the repository) nor
// passed through (a future key's value may not be credential-free -
// refStorage already accepts a URI payload, and git's own manual
// anticipates backends like postgres:// whose URI could carry a
// password).
func sanitizeGitConfig(config, configPath string) (string, error) {
	var kept, extensions []string
	section := "other"
	for _, line := range strings.Split(config, "\n") {
		if header := sectionHeader.FindStringSubmatch(line); header != nil {
			name := strings.ToLower(header[1])
			switch {
			case name == "core":
				section = "core"
				kept = append(kept, line)
			case name == "extensions" || strings.HasPrefix(name, "extensions."):
				if !plainExtensionsHeader.MatchString(line) {
					return "", unavailable("%s, and %q has an [extensions] header with a subsection (%q), which no documented extension key uses - refusing to forward it into the container or silently drop it",
						linkedWorktree, configPath, strings.TrimSpace(line))
				}
				section = "extensions"
				// git accepts a variable written on the same line as
				// its section header, so a key can arrive either on
				// its own line or riding the header.
				inline := strings.TrimSpace(plainExtensionsHeader.ReplaceAllString(line, ""))
				if inline != "" && !isComment(inline) {
					if err := checkExtensionEntry(inline, configPath); err != nil {
						return "", err
					}
					extensions = append(extensions, "\t"+inline)
				}
			default:
				section = "other"
			}
			continue
		}
		switch section {
		case "core":
			kept = append(kept, line)
		case "extensions":
			entry := strings.TrimSpace(line)
			if entry == "" || isComment(entry) {
				continue
			}
			if err := checkExtensionEntry(entry, configPath); err != nil {
				return "", err
			}
			extensions = append(extensions, line)
		}
	}
	if len(extensions) > 0 {
		kept = append(kept, "[extensions]")
		kept = append(kept, extensions...)
	}
	return strings.Join(kept, "\n") + "\n", nil
}

func isComment(line string) bool {
	return strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";")
}

func checkExtensionEntry(entry, configPath string) error {
	fields := configEntry.FindStringSubmatch(entry)
	if fields == nil {
		return unavailable("%s, and %q has an [extensions] line (%q) this sanitizer cannot parse - refusing to guess whether it is credential-free",
			linkedWorktree, configPath, entry)
	}
	key := fields[1]
	if !forwardedExtensionKeys[strings.ToLower(key)] {
		return unavailable("%s, and %q sets extensions.%s, which is not on the sanitizer's allowlist of known credential-free extension keys - refusing to forward it into the container or silently drop it",
			linkedWorktree, configPath, key)
	}
	if strings.EqualFold(key, "refStorage") && strings.Contains(fields[2], "://") {
		return unavailable("%s, and %q sets extensions.refStorage to a URI-form value, whose payload names a host location not visible inside the container - only a bare format name (files, reftable) can be forwarded",
			linkedWorktree, configPath)
	}
	return nil
}
