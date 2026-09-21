package inspector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ResolveRepoRoot returns the absolute top-level directory of the git
// working tree containing path.
func ResolveRepoRoot(path string) (string, error) {
	out, err := runGit(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository: %w", path, err)
	}
	return strings.TrimSpace(out), nil
}

// HeadCommit returns the full SHA of the repo's current HEAD commit - the
// exact commit any result produced by this run is bound to.
func HeadCommit(repoRoot string) (string, error) {
	out, err := runGit(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolving HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// StagingRefForCommit names the non-branch ref that temporarily makes commit
// available to GitHub for its status post. It deliberately cannot be the
// pull-request branch: the status must exist before that branch moves.
func StagingRefForCommit(commit string) string {
	return "refs/inspector/staging/" + commit
}

// ValidatePublicationBranch accepts only a full, unambiguous branch name.
func ValidatePublicationBranch(branch string) error {
	if branch == "" || strings.TrimSpace(branch) != branch {
		return fmt.Errorf("branch name must be non-empty and contain no leading or trailing whitespace")
	}
	if strings.HasPrefix(branch, "refs/") {
		return fmt.Errorf("branch name %q must not include a refs/ prefix", branch)
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("branch name %q must not begin with a hyphen", branch)
	}
	if _, err := runGit(".", "check-ref-format", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("%q is not a valid git branch name: %w", branch, err)
	}
	return nil
}

type trackedWorktreeEntry struct {
	path            string
	mode            os.FileMode
	digest          [sha256.Size]byte
	missing         bool
	gitlink         bool
	gitlinkCommit   string
	submodulePolicy *ValidationPolicy
}

type validationIgnoreFile struct {
	path     string
	contents []byte
}

type ValidationPolicy struct {
	commit          string
	objectFormat    string
	globalExcludes  []byte
	infoExcludes    []byte
	ignoreFiles     []validationIgnoreFile
	trackedTree     []trackedWorktreeEntry
	excludeRunsPath bool
}

func CaptureValidationPolicy(repoRoot string) (ValidationPolicy, error) {
	return captureValidationPolicy(repoRoot, true)
}

func captureValidationPolicy(repoRoot string, excludeRunsPath bool) (ValidationPolicy, error) {
	commit, err := validationHeadCommit(repoRoot)
	if err != nil {
		return ValidationPolicy{}, err
	}
	objectFormat, err := gitObjectFormat(repoRoot)
	if err != nil {
		return ValidationPolicy{}, err
	}
	infoExcludePath, err := gitPath(repoRoot, "info/exclude")
	if err != nil {
		return ValidationPolicy{}, err
	}
	infoExcludes, err := readOptionalFile(infoExcludePath)
	if err != nil {
		return ValidationPolicy{}, fmt.Errorf("reading repository exclude rules: %w", err)
	}
	globalExcludePath, err := effectiveGlobalExcludePath(repoRoot)
	if err != nil {
		return ValidationPolicy{}, err
	}
	globalExcludes, err := readOptionalFile(globalExcludePath)
	if err != nil {
		return ValidationPolicy{}, fmt.Errorf("reading global exclude rules: %w", err)
	}
	trackedTree, err := captureTrackedWorktree(repoRoot)
	if err != nil {
		return ValidationPolicy{}, err
	}
	ignoreFiles, err := captureIgnoreFiles(repoRoot, trackedTree)
	if err != nil {
		return ValidationPolicy{}, err
	}
	currentCommit, err := validationHeadCommit(repoRoot)
	if err != nil {
		return ValidationPolicy{}, err
	}
	if currentCommit != commit {
		return ValidationPolicy{}, fmt.Errorf("HEAD changed while validation state was captured: started at %s, ended at %s", commit, currentCommit)
	}
	return ValidationPolicy{
		commit:          commit,
		objectFormat:    objectFormat,
		globalExcludes:  globalExcludes,
		infoExcludes:    infoExcludes,
		ignoreFiles:     ignoreFiles,
		trackedTree:     trackedTree,
		excludeRunsPath: excludeRunsPath,
	}, nil
}

// ValidatePublicationCommit confirms the checked commit is still HEAD and the
// working tree still contains exactly that commit's code.
func ValidatePublicationCommit(repoRoot, expectedCommit string, policy ValidationPolicy) error {
	if policy.commit != expectedCommit {
		return fmt.Errorf("validation state was captured for commit %s, but inspector checked %s", policy.commit, expectedCommit)
	}
	commit, err := validationHeadCommit(repoRoot)
	if err != nil {
		return err
	}
	if commit != expectedCommit {
		return fmt.Errorf("HEAD changed during inspection: checked %s, now at %s", expectedCommit, commit)
	}
	status, err := workingTreeStatusAtCommit(repoRoot, expectedCommit, policy)
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("working tree changed during inspection, so the passing check was not run against commit %s exactly; commit or stash these files before retrying:\n\n%s", expectedCommit, status)
	}
	return nil
}

var gitPushTimeout = 30 * time.Second

// ValidatePublicationPlatform refuses platforms where a timed-out push cannot
// reliably terminate git's complete process tree.
func ValidatePublicationPlatform() error {
	return validatePublicationPlatform()
}

// PushRefToRemote sends source to the resolved publication remote. Callers use
// an explicit source commit and destination ref rather than a tracking ref, so
// a checked commit is the only object a publication step can move.
func PushRefToRemote(repoRoot, remote, source, destination string) error {
	if _, err := runGitPush(repoRoot, "push", "--porcelain", remote, source+":"+destination); err != nil {
		return fmt.Errorf("pushing %s to %s: %w", source, destination, err)
	}
	return nil
}

// DeleteRefFromRemote removes ref from the resolved publication remote.
func DeleteRefFromRemote(repoRoot, remote, ref string) error {
	if _, err := runGitPush(repoRoot, "push", "--porcelain", remote, ":"+ref); err != nil {
		return fmt.Errorf("deleting temporary staging ref %s: %w", ref, err)
	}
	return nil
}

// githubRemoteRE matches the URL shapes `git remote get-url` returns for
// a github.com remote: HTTPS (with or without a credential prefix or a
// trailing .git), the git@ scp-like form, and the ssh:// form.
var githubRemoteRE = regexp.MustCompile(`^(?:https?://(?:[^@/]+@)?github\.com/|(?:ssh://)?git@github\.com[:/])([^/]+)/(.+?)(?:\.git)?/?$`)
var httpURLCredentialRE = regexp.MustCompile(`(?i)^https?://[^/@\s]+@`)

// PublicationTarget binds the exact push URL to the GitHub repository that
// receives the commit status.
type PublicationTarget struct {
	PushURL     string
	Owner, Repo string
}

// ResolvePublicationTarget resolves origin's effective push URL once so every
// push and the status post use one immutable publication target.
func ResolvePublicationTarget(repoRoot string) (PublicationTarget, error) {
	out, err := runGit(repoRoot, "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return PublicationTarget{}, fmt.Errorf("resolving the 'origin' push remote: %w", err)
	}
	urls := strings.Split(strings.TrimSpace(out), "\n")
	if len(urls) != 1 || strings.TrimSpace(urls[0]) == "" {
		return PublicationTarget{}, fmt.Errorf("origin must have exactly one push URL so refs and status cannot be split across repositories; found %d", len(urls))
	}
	pushURL := strings.TrimSpace(urls[0])
	if hasEmbeddedHTTPCredentials(pushURL) {
		return PublicationTarget{}, fmt.Errorf("origin push remote contains embedded HTTP credentials; use a credential helper or SSH instead")
	}
	m := githubRemoteRE.FindStringSubmatch(pushURL)
	if m == nil {
		return PublicationTarget{}, fmt.Errorf("origin push remote %q is not a github.com URL inspector recognizes", redactURLCredentials(pushURL))
	}
	return PublicationTarget{PushURL: pushURL, Owner: m[1], Repo: m[2]}, nil
}

// RemoteOwnerRepo returns the GitHub owner and repository name for origin's
// publication target.
func RemoteOwnerRepo(repoRoot string) (owner, repo string, err error) {
	target, err := ResolvePublicationTarget(repoRoot)
	if err != nil {
		return "", "", err
	}
	return target.Owner, target.Repo, nil
}

// urlCredentialRE matches the userinfo part of a URL - everything between
// the scheme and the host.
var urlCredentialRE = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]+@`)

// redactURLCredentials strips any embedded credentials from a remote URL
// before it goes into an error message. inspector's errors land in CI and
// pipeline logs, and a remote like https://user:token@example.com/o/r.git
// would otherwise write that token there.
func redactURLCredentials(url string) string {
	return urlCredentialRE.ReplaceAllString(url, "${1}[redacted]@")
}

func hasEmbeddedHTTPCredentials(value string) bool {
	return httpURLCredentialRE.MatchString(value)
}

// WorkingTreeStatus returns differences between HEAD and the worktree plus any
// mutable index flags that could hide tracked changes. It omits untracked files
// in RunsDirName, inspector's own report directory, but still reports tracked
// changes there. The initial check uses the checkout's clean filters so valid
// filtered worktrees compare against HEAD correctly. A captured raw snapshot,
// rather than those mutable filters, protects the post-check comparison.
func WorkingTreeStatus(repoRoot string) (string, error) {
	out, err := runWorktreeGit(repoRoot, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none", "--", ".", ":!"+RunsDirName)
	if err != nil {
		return "", fmt.Errorf("checking working tree status: %w", err)
	}
	trackedRuns, err := runWorktreeGit(repoRoot, "status", "--porcelain", "--untracked-files=no", "--ignore-submodules=none", "--", RunsDirName)
	if err != nil {
		return "", fmt.Errorf("checking tracked report files: %w", err)
	}
	mutableIndex, err := mutableIndexFlags(repoRoot)
	if err != nil {
		return "", err
	}
	return out + trackedRuns + mutableIndex, nil
}

func validationHeadCommit(repoRoot string) (string, error) {
	out, err := runValidationGit(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolving HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func workingTreeStatusAtCommit(repoRoot, commit string, policy ValidationPolicy) (string, error) {
	if policy.commit != commit {
		return "", fmt.Errorf("validation state for %s cannot validate commit %s", policy.commit, commit)
	}
	trackedChanges, err := compareTrackedWorktree(repoRoot, policy.trackedTree)
	if err != nil {
		return "", err
	}
	untracked, err := isolatedUntrackedFiles(repoRoot, commit, policy)
	if err != nil {
		return "", err
	}
	indexChanges, err := runValidationGit(repoRoot, "diff-index", "--cached", "--name-status", commit, "--")
	if err != nil {
		return "", fmt.Errorf("checking staged changes: %w", err)
	}
	mutableIndex, err := mutableIndexFlags(repoRoot)
	if err != nil {
		return "", err
	}
	return trackedChanges + untracked + indexChanges + mutableIndex, nil
}

func captureTrackedWorktree(repoRoot string) ([]trackedWorktreeEntry, error) {
	out, err := runValidationGit(repoRoot, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, fmt.Errorf("listing tracked files: %w", err)
	}
	var snapshot []trackedWorktreeEntry
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		metadata, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || fields[2] != "0" {
			return nil, fmt.Errorf("cannot snapshot unresolved index entry %q", record)
		}
		entry, err := snapshotTrackedPath(repoRoot, path, fields[0] == "160000", fields[1])
		if err != nil {
			return nil, err
		}
		snapshot = append(snapshot, entry)
	}
	return snapshot, nil
}

func snapshotTrackedPath(repoRoot, path string, gitlink bool, gitlinkCommit string) (trackedWorktreeEntry, error) {
	entry, err := readTrackedPath(repoRoot, path, gitlink)
	if err != nil {
		return trackedWorktreeEntry{}, err
	}
	if !gitlink || entry.missing {
		return entry, nil
	}
	if entry.gitlinkCommit != gitlinkCommit {
		return trackedWorktreeEntry{}, fmt.Errorf("tracked submodule %s is at %s instead of recorded commit %s", path, entry.gitlinkCommit, gitlinkCommit)
	}
	policy, err := captureValidationPolicy(filepath.Join(repoRoot, filepath.FromSlash(path)), false)
	if err != nil {
		return trackedWorktreeEntry{}, fmt.Errorf("capturing tracked submodule %s: %w", path, err)
	}
	entry.submodulePolicy = &policy
	return entry, nil
}

func readTrackedPath(repoRoot, path string, gitlink bool) (trackedWorktreeEntry, error) {
	entry := trackedWorktreeEntry{path: path, gitlink: gitlink}
	fullPath := filepath.Join(repoRoot, filepath.FromSlash(path))
	info, err := os.Lstat(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		entry.missing = true
		return entry, nil
	}
	if err != nil {
		return trackedWorktreeEntry{}, fmt.Errorf("reading tracked path %s: %w", path, err)
	}
	entry.mode = info.Mode() & (os.ModeType | 0o111)
	if gitlink {
		if !info.IsDir() {
			return trackedWorktreeEntry{}, fmt.Errorf("tracked submodule %s is not a directory", path)
		}
		head, err := runValidationGit(fullPath, "rev-parse", "HEAD")
		if err != nil {
			return trackedWorktreeEntry{}, fmt.Errorf("reading tracked submodule %s: %w", path, err)
		}
		entry.gitlinkCommit = strings.TrimSpace(head)
		return entry, nil
	}
	hash := sha256.New()
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(fullPath)
		if err != nil {
			return trackedWorktreeEntry{}, fmt.Errorf("reading tracked symlink %s: %w", path, err)
		}
		_, _ = hash.Write([]byte(target))
	} else {
		if !info.Mode().IsRegular() {
			return trackedWorktreeEntry{}, fmt.Errorf("tracked path %s is not a regular file or symlink", path)
		}
		file, err := os.Open(fullPath)
		if err != nil {
			return trackedWorktreeEntry{}, fmt.Errorf("reading tracked file %s: %w", path, err)
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return trackedWorktreeEntry{}, fmt.Errorf("reading tracked file %s: %w", path, copyErr)
		}
		if closeErr != nil {
			return trackedWorktreeEntry{}, fmt.Errorf("closing tracked file %s: %w", path, closeErr)
		}
	}
	copy(entry.digest[:], hash.Sum(nil))
	return entry, nil
}

func compareTrackedWorktree(repoRoot string, snapshot []trackedWorktreeEntry) (string, error) {
	var changes strings.Builder
	for _, expected := range snapshot {
		actual, err := readTrackedPath(repoRoot, expected.path, expected.gitlink)
		if err != nil {
			return "", err
		}
		if !sameTrackedPath(actual, expected) {
			code := " M "
			if actual.missing {
				code = " D "
			}
			fmt.Fprintf(&changes, "%s%s\n", code, expected.path)
			continue
		}
		if expected.submodulePolicy != nil {
			submoduleRoot := filepath.Join(repoRoot, filepath.FromSlash(expected.path))
			submoduleStatus, err := workingTreeStatusAtCommit(submoduleRoot, expected.gitlinkCommit, *expected.submodulePolicy)
			if err != nil {
				return "", fmt.Errorf("validating tracked submodule %s: %w", expected.path, err)
			}
			changes.WriteString(prefixStatusPaths(submoduleStatus, expected.path+"/"))
		}
	}
	return changes.String(), nil
}

func sameTrackedPath(actual, expected trackedWorktreeEntry) bool {
	return actual.path == expected.path &&
		actual.mode == expected.mode &&
		actual.digest == expected.digest &&
		actual.missing == expected.missing &&
		actual.gitlink == expected.gitlink &&
		actual.gitlinkCommit == expected.gitlinkCommit
}

func prefixStatusPaths(status, prefix string) string {
	var prefixed strings.Builder
	for _, line := range strings.Split(status, "\n") {
		if line == "" {
			continue
		}
		switch {
		case len(line) >= 3 && line[2] == ' ':
			fmt.Fprintf(&prefixed, "%s%s%s\n", line[:3], prefix, line[3:])
		case strings.HasPrefix(line, "mutable index flag hides tracked content: "):
			fmt.Fprintf(&prefixed, "mutable index flag hides tracked content: %s%s\n", prefix, strings.TrimPrefix(line, "mutable index flag hides tracked content: "))
		case strings.Contains(line, "\t"):
			before, path, _ := strings.Cut(line, "\t")
			fmt.Fprintf(&prefixed, "%s\t%s%s\n", before, prefix, path)
		default:
			fmt.Fprintf(&prefixed, "%s: %s\n", strings.TrimSuffix(prefix, "/"), line)
		}
	}
	return prefixed.String()
}

func captureIgnoreFiles(repoRoot string, trackedTree []trackedWorktreeEntry) ([]validationIgnoreFile, error) {
	paths := make(map[string]bool)
	for _, entry := range trackedTree {
		if filepath.Base(filepath.FromSlash(entry.path)) == ".gitignore" && !entry.missing && !entry.gitlink && entry.mode.IsRegular() {
			paths[entry.path] = true
		}
	}
	out, err := runValidationGit(repoRoot, "ls-files", "--others", "-z", "--", ".gitignore", ":(glob)**/.gitignore")
	if err != nil {
		return nil, fmt.Errorf("listing untracked exclude files: %w", err)
	}
	for _, path := range strings.Split(out, "\x00") {
		if path != "" {
			paths[path] = true
		}
	}
	ignoreFiles := make([]validationIgnoreFile, 0, len(paths))
	for path := range paths {
		fullPath := filepath.Join(repoRoot, filepath.FromSlash(path))
		info, err := os.Lstat(fullPath)
		if err != nil {
			return nil, fmt.Errorf("reading exclude file %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		contents, err := os.ReadFile(fullPath)
		if err != nil {
			return nil, fmt.Errorf("reading exclude file %s: %w", path, err)
		}
		ignoreFiles = append(ignoreFiles, validationIgnoreFile{path: path, contents: contents})
	}
	return ignoreFiles, nil
}

func isolatedUntrackedFiles(repoRoot, commit string, policy ValidationPolicy) (string, error) {
	objectDir, err := gitObjectDirectory(repoRoot)
	if err != nil {
		return "", err
	}
	validationRepo, err := os.MkdirTemp("", "inspector-validation-")
	if err != nil {
		return "", fmt.Errorf("creating isolated validation repository: %w", err)
	}
	defer os.RemoveAll(validationRepo)

	env := validationGitEnv()
	if err := initBareRepository(validationRepo, policy.objectFormat, env); err != nil {
		return "", fmt.Errorf("creating isolated validation repository: %w", err)
	}
	if err := os.WriteFile(filepath.Join(validationRepo, "info", "exclude"), policy.infoExcludes, 0o600); err != nil {
		return "", fmt.Errorf("installing repository exclude rules: %w", err)
	}
	globalExcludesPath := filepath.Join(validationRepo, "global-excludes")
	if err := os.WriteFile(globalExcludesPath, policy.globalExcludes, 0o600); err != nil {
		return "", fmt.Errorf("installing global exclude rules: %w", err)
	}
	env = append(env,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+objectDir,
		"GIT_INDEX_FILE="+filepath.Join(validationRepo, "validation-index"),
	)
	baseArgs := []string{"--git-dir=" + validationRepo, "-c", "core.bare=false", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.excludesFile=" + globalExcludesPath}
	readTree := exec.Command("git", append(baseArgs, "read-tree", commit)...)
	readTree.Env = env
	if _, err := gitOutput(readTree, []string{"read-tree", commit}); err != nil {
		return "", fmt.Errorf("reading checked commit tree: %w", err)
	}
	currentArgs := append(append([]string{}, baseArgs...), "--work-tree="+repoRoot)
	untrackedCmd := exec.Command("git", append(currentArgs, "ls-files", "--others", "-z", "--", ".")...)
	untrackedCmd.Env = env
	out, err := gitOutput(untrackedCmd, []string{"ls-files", "--others", "-z"})
	if err != nil {
		return "", fmt.Errorf("listing untracked files: %w", err)
	}
	candidates := splitNullPaths(out)
	if len(candidates) == 0 {
		return "", nil
	}

	frozenWorktree, err := os.MkdirTemp("", "inspector-validation-worktree-")
	if err != nil {
		return "", fmt.Errorf("creating frozen validation worktree: %w", err)
	}
	defer os.RemoveAll(frozenWorktree)
	for _, path := range candidates {
		if err := writeFrozenValidationFile(frozenWorktree, path, nil); err != nil {
			return "", err
		}
	}
	for _, ignoreFile := range policy.ignoreFiles {
		if err := writeFrozenValidationFile(frozenWorktree, ignoreFile.path, ignoreFile.contents); err != nil {
			return "", err
		}
	}

	frozenArgs := append(append([]string{}, baseArgs...), "--work-tree="+frozenWorktree)
	unignoredCmd := exec.Command("git", append(frozenArgs, "ls-files", "--others", "--exclude-standard", "-z", "--", ".")...)
	unignoredCmd.Env = env
	out, err = gitOutput(unignoredCmd, []string{"ls-files", "--others", "--exclude-standard", "-z"})
	if err != nil {
		return "", fmt.Errorf("checking untracked files against captured ignore rules: %w", err)
	}
	var status strings.Builder
	for _, path := range splitNullPaths(out) {
		if policy.excludeRunsPath && (path == RunsDirName || strings.HasPrefix(path, RunsDirName+"/")) {
			continue
		}
		fmt.Fprintf(&status, "?? %s\n", path)
	}
	return status.String(), nil
}

func splitNullPaths(out string) []string {
	var paths []string
	for _, path := range strings.Split(out, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func writeFrozenValidationFile(root, path string, contents []byte) error {
	localPath := filepath.FromSlash(path)
	cleanPath := filepath.Clean(localPath)
	if filepath.IsAbs(localPath) || cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid repository path %q while freezing validation rules", path)
	}
	fullPath := filepath.Join(root, localPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		return fmt.Errorf("creating frozen validation directory for %s: %w", path, err)
	}
	if err := os.WriteFile(fullPath, contents, 0o600); err != nil {
		return fmt.Errorf("writing frozen validation path %s: %w", path, err)
	}
	return nil
}

func mutableIndexFlags(repoRoot string) (string, error) {
	out, err := runValidationGit(repoRoot, "ls-files", "-v", "-z")
	if err != nil {
		return "", fmt.Errorf("checking mutable index flags: %w", err)
	}
	var flagged strings.Builder
	for _, entry := range strings.Split(out, "\x00") {
		if len(entry) < 3 || entry[1] != ' ' {
			continue
		}
		if entry[0] == 'S' || entry[0] >= 'a' && entry[0] <= 'z' {
			fmt.Fprintf(&flagged, "mutable index flag hides tracked content: %s\n", entry[2:])
		}
	}
	return flagged.String(), nil
}

// runGit returns only git's stdout. Its stderr is kept separate and used
// solely for the error message - folding it into the value would let a
// warning git prints on an otherwise successful command be parsed as a
// commit SHA or as a dirty working tree.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return gitOutput(cmd, args)
}

func runValidationGit(dir string, args ...string) (string, error) {
	gitArgs := append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "-C", dir}, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Env = validationGitEnv()
	return gitOutput(cmd, args)
}

func runWorktreeGit(dir string, args ...string) (string, error) {
	gitArgs := append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "-C", dir}, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Env = nonInteractiveGitEnv()
	return gitOutput(cmd, args)
}

func runGitPush(dir string, args ...string) (string, error) {
	for _, arg := range args {
		if hasEmbeddedHTTPCredentials(arg) {
			return "", fmt.Errorf("git push target contains embedded HTTP credentials; use a credential helper or SSH instead")
		}
	}
	if err := ValidatePublicationPlatform(); err != nil {
		return "", err
	}

	objectDir, err := gitObjectDirectory(dir)
	if err != nil {
		return "", err
	}
	publicationRepo, err := os.MkdirTemp("", "inspector-publication-")
	if err != nil {
		return "", fmt.Errorf("creating isolated publication repository: %w", err)
	}
	defer os.RemoveAll(publicationRepo)

	objectFormat, err := gitObjectFormat(dir)
	if err != nil {
		return "", err
	}
	env := publicationGitEnv(objectDir)
	if err := initBareRepository(publicationRepo, objectFormat, env); err != nil {
		return "", fmt.Errorf("creating isolated publication repository: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitPushTimeout)
	defer cancel()

	gitArgs := append([]string{"--git-dir=" + publicationRepo, "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = time.Second
	cmd.Env = env
	out, err := gitOutput(cmd, args)
	if ctx.Err() == context.DeadlineExceeded {
		command := redactURLCredentials(strings.Join(args, " "))
		return "", fmt.Errorf("git %s timed out after %s", command, gitPushTimeout)
	}
	return out, err
}

func gitObjectDirectory(repoRoot string) (string, error) {
	return gitPath(repoRoot, "objects")
}

func gitPath(repoRoot, name string) (string, error) {
	out, err := runValidationGit(repoRoot, "rev-parse", "--git-path", name)
	if err != nil {
		return "", fmt.Errorf("resolving repository git path %s: %w", name, err)
	}
	path := strings.TrimSpace(out)
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	return filepath.Clean(path), nil
}

func gitObjectFormat(repoRoot string) (string, error) {
	out, err := runValidationGit(repoRoot, "rev-parse", "--show-object-format=storage")
	if err != nil {
		return "", fmt.Errorf("resolving repository object format: %w", err)
	}
	format := strings.TrimSpace(out)
	if format != "sha1" && format != "sha256" {
		return "", fmt.Errorf("repository uses unsupported object format %q", format)
	}
	return format, nil
}

func initBareRepository(path, objectFormat string, env []string) error {
	args := []string{"init", "--bare", "--quiet", "--object-format=" + objectFormat, path}
	cmd := exec.Command("git", args...)
	cmd.Env = env
	_, err := gitOutput(cmd, args)
	return err
}

func effectiveGlobalExcludePath(repoRoot string) (string, error) {
	args := []string{"config", "--path", "--get", "core.excludesFile"}
	cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...)
	cmd.Env = nonInteractiveGitEnv()
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err == nil {
		path := strings.TrimSpace(string(out))
		if path == "" {
			return "", nil
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(repoRoot, path)
		}
		return filepath.Clean(path), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		xdgConfigHome := os.Getenv("XDG_CONFIG_HOME")
		if filepath.IsAbs(xdgConfigHome) {
			return filepath.Join(xdgConfigHome, "git", "ignore"), nil
		}
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", nil
		}
		return filepath.Join(home, ".config", "git", "ignore"), nil
	}
	detail := redactURLCredentials(strings.TrimSpace(errBuf.String()))
	return "", fmt.Errorf("resolving global exclude file: git %s: %w: %s", strings.Join(args, " "), err, detail)
}

func readOptionalFile(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return contents, err
}

func gitOutput(cmd *exec.Cmd, args []string) (string, error) {
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		command := redactURLCredentials(strings.Join(args, " "))
		detail := redactURLCredentials(strings.TrimSpace(errBuf.String()))
		return "", fmt.Errorf("git %s: %w: %s", command, err, detail)
	}
	return string(out), nil
}

func nonInteractiveGitEnv() []string {
	blocked := map[string]bool{
		"GITHUB_TOKEN":                     true,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_ASKPASS":                      true,
		"GIT_COMMON_DIR":                   true,
		"GIT_ATTR_NOSYSTEM":                true,
		"GIT_CONFIG_COUNT":                 true,
		"GIT_CONFIG_GLOBAL":                true,
		"GIT_CONFIG_NOSYSTEM":              true,
		"GIT_CONFIG_PARAMETERS":            true,
		"GIT_CONFIG_SYSTEM":                true,
		"GIT_DIR":                          true,
		"GIT_INDEX_FILE":                   true,
		"GIT_OBJECT_DIRECTORY":             true,
		"GIT_OPTIONAL_LOCKS":               true,
		"GIT_SSH_COMMAND":                  true,
		"GIT_TERMINAL_PROMPT":              true,
		"GIT_WORK_TREE":                    true,
		"GCM_INTERACTIVE":                  true,
	}
	env := make([]string, 0, len(os.Environ())+4)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if !blocked[name] && !strings.HasPrefix(name, "GIT_CONFIG_KEY_") && !strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			env = append(env, item)
		}
	}
	return append(env,
		"GIT_ASKPASS=true",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=Never",
	)
}

func validationGitEnv() []string {
	return append(nonInteractiveGitEnv(),
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
	)
}

func publicationGitEnv(objectDir string) []string {
	return append(nonInteractiveGitEnv(), "GIT_ALTERNATE_OBJECT_DIRECTORIES="+objectDir)
}
