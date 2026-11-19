package inspector

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/inactdev/inspector/internal/container"
)

// ResolveBuildArgs reads every build argument the project declared out of
// the commit under check, returning the resolved pairs sorted by name -
// which is the order they are passed to docker and hashed into the check
// image's tag, so neither depends on a Go map's iteration order.
//
// Each value is read from git's own object for that path in commit, never
// off the working tree. Run already refuses a dirty tree, so for a tracked
// file the two hold the same bytes; reading the commit closes the two gaps
// that refusal leaves. A gitignored file is ignored rather than dirty, so
// it would otherwise have been able to set the version the check image was
// built with while the commit declared nothing of the sort - the exact
// kind of invisible input the content-derived tag exists to rule out. And
// a tracked symbolic link reads as its own link text rather than as
// whatever it points at, so a declared path cannot reach anything the
// commit does not contain.
//
// Every failure wraps container.ErrBuildArgUnavailable and names both the
// argument and the file, because that pair is what the person reading the
// refusal has to go and look at. None of them is a red: a project whose
// declared environment inspector could not assemble has not been judged.
func ResolveBuildArgs(repoRoot, commit string, sources map[string]BuildArgSource) ([]container.BuildArg, error) {
	if len(sources) == 0 {
		// Nil rather than an empty slice, so a project declaring none is
		// exactly a project from before this key existed - same tag, same
		// docker command line.
		return nil, nil
	}

	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)

	args := make([]container.BuildArg, 0, len(names))
	for _, name := range names {
		value, err := resolveBuildArg(repoRoot, commit, name, sources[name])
		if err != nil {
			return nil, err
		}
		args = append(args, container.BuildArg{Name: name, Value: value})
	}
	// Each name and value was already checked as it was read, where the
	// refusal could also name the file it came from; this is the whole
	// list checked by the rule the build itself applies, so nothing can
	// leave here that the build would then refuse.
	if err := container.ValidateBuildArgs(args); err != nil {
		return nil, err
	}
	return args, nil
}

// resolveBuildArg reads one declared argument's value out of commit.
func resolveBuildArg(repoRoot, commit, name string, source BuildArgSource) (string, error) {
	// The name is refused before its file is ever read: a name inspector
	// will not pass to docker cannot become a build argument whatever that
	// file holds.
	if err := container.ValidateBuildArgName(name); err != nil {
		return "", err
	}
	path, err := commitRelativePath(name, source.File)
	if err != nil {
		return "", err
	}
	content, err := readCommittedFile(repoRoot, commit, name, path)
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(source.Pattern) == "" {
		value, ok := firstNonEmptyLine(content)
		if !ok {
			return "", fmt.Errorf("%w: build argument %s reads %s, which holds no non-empty line - with no \"pattern\", the value is that file's first non-empty line, trimmed", container.ErrBuildArgUnavailable, name, path)
		}
		return value, validateResolvedValue(name, path, "", value)
	}

	pattern, err := regexp.Compile(source.Pattern)
	if err != nil {
		return "", fmt.Errorf("%w: build argument %s declares a pattern for %s that is not a valid regular expression: %v", container.ErrBuildArgUnavailable, name, path, err)
	}
	if groups := pattern.NumSubexp(); groups != 1 {
		// Exactly one, rather than "the first": there is no rule for which
		// of several groups would be the value, and guessing one would
		// silently build a different image than the project meant.
		return "", fmt.Errorf("%w: build argument %s declares a pattern for %s with %d capture groups; it must have exactly one, holding the value - use a non-capturing group (?:...) for the rest", container.ErrBuildArgUnavailable, name, path, groups)
	}
	// Matched against the whole file rather than line by line, because the
	// value is not always on the line that identifies it: Gemfile.lock's
	// "BUNDLED WITH" is followed by the version on the next line. Go's
	// regexp is RE2, so a project's own pattern cannot backtrack the run
	// into a hang; (?m) and (?s) are there for line-anchored and
	// dot-matches-newline matching.
	match := pattern.FindStringSubmatch(content)
	if match == nil {
		return "", fmt.Errorf("%w: build argument %s reads %s with pattern %q, which matches nothing in that file as the commit under check holds it", container.ErrBuildArgUnavailable, name, path, source.Pattern)
	}
	value := strings.TrimSpace(match[1])
	return value, validateResolvedValue(name, path, source.Pattern, value)
}

// validateResolvedValue applies the build's own value rule to a value just
// read, so the refusal names where it came from rather than only which
// argument it was for.
func validateResolvedValue(name, path, pattern, value string) error {
	err := container.ValidateBuildArgValue(name, value)
	if err == nil {
		return nil
	}
	if pattern == "" {
		return fmt.Errorf("%w (read from %s)", err, path)
	}
	return fmt.Errorf("%w (read from %s with pattern %q)", err, path, pattern)
}

// commitRelativePath turns a declared file into the path git names inside
// a commit: slash-separated, relative to the repository root, and inside
// it. An absolute path or one climbing out with ".." is refused by name
// rather than handed to git, which would reject it in words about its own
// internals - and the value a build argument carries has to come from the
// repository under check, which is the only thing the commit binds.
func commitRelativePath(name, file string) (string, error) {
	path := strings.TrimSpace(file)
	switch {
	case path == "":
		return "", fmt.Errorf("%w: build argument %s names no \"file\" to read its value from", container.ErrBuildArgUnavailable, name)
	case strings.HasPrefix(path, "/"):
		return "", fmt.Errorf("%w: build argument %s reads %q, which is an absolute path - the value a build argument carries comes from the repository under check, so name the file relative to the repository root, as git records it", container.ErrBuildArgUnavailable, name, file)
	case strings.Contains(path, ":"):
		// A colon is what separates a commit from a path in git's own
		// object syntax, which is how this file is read. A path carrying
		// one is refused rather than handed over to be split somewhere
		// inspector did not choose.
		return "", fmt.Errorf("%w: build argument %s reads %q, which contains a colon - git's own commit:path syntax uses it as a separator, so inspector will not read that path out of the commit", container.ErrBuildArgUnavailable, name, file)
	case strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../"):
		// "./" is how git spells "relative to the current directory"
		// rather than to the repository root, so it is refused rather
		// than quietly stripped: the two mean different files.
		return "", fmt.Errorf("%w: build argument %s reads %q; name it relative to the repository root, with no leading \"./\" or \"../\"", container.ErrBuildArgUnavailable, name, file)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." || segment == "." || segment == "" {
			return "", fmt.Errorf("%w: build argument %s reads %q, which is not a plain path inside the repository under check", container.ErrBuildArgUnavailable, name, file)
		}
	}
	return path, nil
}

// MaxBuildArgFileBytes bounds how large a file a build argument reads may
// be. The files this key exists for are version files and lockfiles; the
// bound is here because the whole object is read into memory to be matched
// against, so a declared path aimed at a repository's large tracked asset
// refuses by name rather than making inspector itself the problem.
const MaxBuildArgFileBytes = 4 << 20

// readCommittedFile returns what the commit records at path, refusing when
// the commit has nothing there, has something that is not a file, or has
// one too large to read.
//
// Git's object rather than the file on disk, for the reasons
// ResolveBuildArgs states. Its size is asked for first, which is also how
// a path the commit does not hold is found: `cat-file` answers both
// questions about an object without producing its content. Then `blob`
// rather than `-p`, so a path naming a directory is refused by git rather
// than returning that tree's listing as if it were a value.
func readCommittedFile(repoRoot, commit, name, path string) (string, error) {
	// Every object name here is built from the commit inspector resolved
	// itself and a path already checked to be a plain one inside the
	// repository, so nothing can be read as an option or reach outside the
	// commit.
	object := commit + ":" + path
	out, err := runValidationGit(repoRoot, "cat-file", "-s", object)
	if err != nil {
		return "", fmt.Errorf("%w: build argument %s reads %s, which is not a file in the commit under check (%v)", container.ErrBuildArgUnavailable, name, path, err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return "", fmt.Errorf("%w: build argument %s reads %s, and git answered %q when asked how large it is in the commit under check", container.ErrBuildArgUnavailable, name, path, strings.TrimSpace(out))
	}
	if size > MaxBuildArgFileBytes {
		return "", fmt.Errorf("%w: build argument %s reads %s, which is %d bytes in the commit under check - past the %d a file holding a build argument's value may be, since the whole of it is read to find that value", container.ErrBuildArgUnavailable, name, path, size, MaxBuildArgFileBytes)
	}
	content, err := runValidationGit(repoRoot, "cat-file", "blob", object)
	if err != nil {
		return "", fmt.Errorf("%w: build argument %s reads %s, which is not a file in the commit under check (%v)", container.ErrBuildArgUnavailable, name, path, err)
	}
	return content, nil
}

// firstNonEmptyLine is the value a source with no pattern carries: the
// file's first line with anything on it, trimmed. That is what
// .ruby-version, .nvmrc, .node-version and their kin hold - a bare
// version, often with a trailing newline, sometimes after a comment line.
func firstNonEmptyLine(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed, true
		}
	}
	return "", false
}
