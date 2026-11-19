package container

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ErrBuildArgUnavailable means a build argument the project declared cannot
// be handed to its check image build: the name is not one inspector will
// pass to docker, or the value read for it is not one it will put on a
// command line and into an image tag. It wraps ErrProjectImageUnavailable,
// so a caller that only knows the general sentinel still refuses - the
// image the check command was to run in was never built, and a build run
// with the wrong version of a toolchain would judge a project nobody asked
// about.
var ErrBuildArgUnavailable = fmt.Errorf("%w: a declared build argument could not be resolved", ErrProjectImageUnavailable)

// BuildArg is one resolved build argument: the name a Dockerfile declares
// with ARG, and the value read for it out of the repository under check.
//
// Unlike a build secret, both halves are carried, printed and recorded.
// That is the point of the two being different keys: a value that may not
// appear in an image's metadata, a console line or a report is a secret
// and belongs under buildSecrets, where inspector never holds it at all.
type BuildArg struct {
	Name  string
	Value string
}

// buildArgNamePattern is what a build argument name may look like:
// letters, digits and underscores, which is the whole of what an ARG name
// needs. Nothing else, and no escaping, because the name is joined to its
// value with "=" in a single docker argument - a name carrying an equals
// sign would move the boundary between the two, and one carrying a space
// or a leading dash would be read as something other than this flag's
// value. Refusing the character class is the one form that cannot be
// smuggled through.
var buildArgNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// MaxBuildArgValueBytes bounds how long a resolved build argument value
// may be. These are versions and other short facts a repository states
// about itself - a bound exists because the value is read by a
// project-supplied pattern, and a capture group that swallows a whole
// lockfile must refuse by name rather than become a megabyte-long docker
// argument that ends up in an image's metadata and in every report.
const MaxBuildArgValueBytes = 1024

// ValidateBuildArgs reports whether these resolved pairs can be handed to
// a build: every name one inspector will pass to docker, every value one
// it will put on a command line, in an image tag and in a report, and no
// name twice.
//
// BuildProjectImage applies it to whatever it is handed, so "no
// unvalidated name or value reaches a docker flag" is a property of the
// function that builds rather than of its callers' discipline. The caller
// that reads these values out of the commit under check applies the same
// two rules one at a time - ValidateBuildArgName before it reads a file at
// all, ValidateBuildArgValue once it has - so its own refusal can name the
// file the value came from.
func ValidateBuildArgs(args []BuildArg) error {
	seen := make(map[string]bool, len(args))
	for _, arg := range args {
		if err := ValidateBuildArgName(arg.Name); err != nil {
			return err
		}
		if seen[arg.Name] {
			// Two values for one name is a question with no answer -
			// unlike a repeated build secret, which says the same thing
			// twice and is collapsed.
			return fmt.Errorf("%w: %s is declared more than once, and inspector will not choose between two values for one build argument", ErrBuildArgUnavailable, arg.Name)
		}
		seen[arg.Name] = true
		if err := ValidateBuildArgValue(arg.Name, arg.Value); err != nil {
			return err
		}
	}
	return nil
}

// ValidateBuildArgName reports whether name is one inspector will pass to
// docker. Exported for the caller that refuses a declared name before
// reading anything for it: a name that cannot become a build argument
// cannot become one whatever the file it points at holds.
func ValidateBuildArgName(name string) error {
	if !buildArgNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q is not a name inspector will pass to docker - a build argument names an ARG, so it may hold only letters, digits and underscores", ErrBuildArgUnavailable, name)
	}
	return nil
}

// ValidateBuildArgValue reports whether value is one inspector will pass
// to docker, hash into an image tag, print and record for name.
func ValidateBuildArgValue(name, value string) error {
	switch {
	case value == "":
		return fmt.Errorf("%w: %s resolved to an empty value, and inspector does not build with one - an empty build argument is how a version silently becomes \"latest\"", ErrBuildArgUnavailable, name)
	case len(value) > MaxBuildArgValueBytes:
		return fmt.Errorf("%w: %s resolved to a %d-byte value, past the %d bytes a build argument may hold - it is meant to carry a version, and this value also goes into the image's tag, its metadata and this run's report", ErrBuildArgUnavailable, name, len(value), MaxBuildArgValueBytes)
	case !utf8.ValidString(value):
		return fmt.Errorf("%w: %s resolved to a value that is not valid UTF-8, so it is not text inspector will pass to docker", ErrBuildArgUnavailable, name)
	}
	for _, r := range value {
		// Control characters, newline and tab included: the value is
		// joined into one docker argument, folded into the image tag line
		// by line, and printed in the run's own output, and every one of
		// those reads wrongly if it can contain a line break.
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s resolved to a value containing a control character (%q), which inspector will not pass to docker, hash into an image tag or print - a pattern that captures a line break usually means the capture group is wider than it was meant to be", ErrBuildArgUnavailable, name, r)
		}
	}
	return nil
}

// buildArgFlags is the docker build arguments for resolved build argument
// pairs: one `--build-arg NAME=value` each, in the order given, which the
// caller keeps sorted by name so neither this command line nor the image
// tag depends on a Go map's iteration order.
//
// Unlike a build secret, the value is on the command line and in the built
// image's own metadata. That is docker's behavior, not a choice made here,
// and it is why these two keys stay separate.
func buildArgFlags(args []BuildArg) []string {
	flags := make([]string, 0, 2*len(args))
	for _, arg := range args {
		flags = append(flags, "--build-arg", arg.Name+"="+arg.Value)
	}
	return flags
}

// substituteBuildArgs expands the declared build arguments in a FROM
// instruction's image reference - `${NAME}` and `$NAME`, nothing else -
// and reports whether every variable it found was one of them.
//
// Deliberately not a shell: `${NAME:-default}` and `${NAME:+set}` are
// forms docker accepts and this does not, and a name no buildArgs entry
// declares is left as it stands. Both leave a `$` behind, which is what
// parseDockerfile refuses by name. That asymmetry is the safe one - the
// failure being avoided is inspector resolving and hashing one base image
// while docker builds the project from another.
func substituteBuildArgs(ref string, values map[string]string) string {
	var expanded strings.Builder
	for i := 0; i < len(ref); {
		if ref[i] != '$' {
			expanded.WriteByte(ref[i])
			i++
			continue
		}
		name, next := buildArgReference(ref, i)
		value, declared := values[name]
		if name == "" || !declared {
			// Left exactly as written, including the braces, so the
			// refusal quotes the line the project wrote.
			expanded.WriteString(ref[i:next])
			i = next
			continue
		}
		expanded.WriteString(value)
		i = next
	}
	return expanded.String()
}

// buildArgReference reads the variable reference starting at the `$` at
// index start, returning the name it holds and the index just past the
// whole reference. An empty name means whatever is there is not a
// reference this understands - a bare `$`, an unterminated `${`, or a
// `${...}` holding anything but a name, such as `${NAME:-default}`.
func buildArgReference(ref string, start int) (name string, next int) {
	i := start + 1
	if i < len(ref) && ref[i] == '{' {
		end := strings.IndexByte(ref[i:], '}')
		if end < 0 {
			return "", len(ref)
		}
		closing := i + end
		inner := ref[i+1 : closing]
		if !buildArgNamePattern.MatchString(inner) {
			return "", closing + 1
		}
		return inner, closing + 1
	}
	end := i
	for end < len(ref) && isBuildArgNameByte(ref[end]) {
		end++
	}
	return ref[i:end], end
}

func isBuildArgNameByte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

// buildArgValues is the name-to-value map substitution reads, built from
// the resolved pairs the build was handed.
func buildArgValues(args []BuildArg) map[string]string {
	if len(args) == 0 {
		return nil
	}
	values := make(map[string]string, len(args))
	for _, arg := range args {
		values[arg.Name] = arg.Value
	}
	return values
}
