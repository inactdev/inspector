package container

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ErrBuildSecretUnavailable means a build secret the project declared
// cannot be handed to its check image build: the name is not one inspector
// will pass to docker, or inspector's own environment does not carry it.
// It wraps ErrProjectImageUnavailable, so a caller that only knows the
// general sentinel still refuses - the image the check command was to run
// in was never built, and a build run without a credential the project
// said it needs would fail somewhere inside its own log instead.
var ErrBuildSecretUnavailable = fmt.Errorf("%w: a declared build secret could not be handed to the build", ErrProjectImageUnavailable)

// buildSecretNamePattern is what a buildSecrets entry may look like:
// letters, digits and underscores, which is the whole of what an
// environment variable name needs. Nothing else, and no escaping, because
// the name goes into a comma-separated docker flag value
// (`id=NAME,env=NAME`) where a comma, an equals sign or a space would
// change which flag docker reads. Refusing the character class is the one
// form that cannot be smuggled through.
var buildSecretNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// resolveBuildSecrets validates the names a project declared and confirms
// inspector's own environment carries each one, returning the names to
// pass to docker - repeats collapsed, first-mentioned order kept.
//
// It deliberately returns names and never values. The value is read here
// exactly once, to ask whether it is there, and dropped: docker is told
// which variable of its own environment to read (`env=NAME`), so the
// credential never passes through a string inspector formats, logs,
// reports or posts, and there is no field anywhere holding one for a
// future %v to print.
//
// Unset and empty are one case, the same ruling the gate's own sync makes:
// a secret that does not exist in CI arrives as an empty variable rather
// than an unset one.
func resolveBuildSecrets(names []string) ([]string, error) {
	resolved := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if !buildSecretNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%w: %q is not a name inspector will pass to docker - a build secret names an environment variable, so it may hold only letters, digits and underscores", ErrBuildSecretUnavailable, name)
		}
		value, set := os.LookupEnv(name)
		if !set {
			return nil, fmt.Errorf("%w: %s is declared as a build secret but is not set in inspector's own environment, and the build is never run without it - export it where inspector runs", ErrBuildSecretUnavailable, name)
		}
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%w: %s is declared as a build secret but is empty in inspector's own environment, which counts as missing (a secret that does not exist arrives empty rather than unset), and the build is never run without it", ErrBuildSecretUnavailable, name)
		}
		if seen[name] {
			// Twice in the list says the same thing once. Collapsed
			// rather than refused or passed twice: what docker does with
			// two --secret flags carrying one id is not documented, and
			// inspector does not build a call on undocumented behavior.
			continue
		}
		seen[name] = true
		resolved = append(resolved, name)
	}
	if len(resolved) == 0 {
		// Nil rather than an empty slice, so a project declaring none is
		// exactly a project from before this key existed.
		return nil, nil
	}
	return resolved, nil
}

// ValidateBuildSecrets reports whether every name a project declared can
// be handed to a build, by exactly the rule the build itself applies -
// without a daemon, an image, or anything built.
//
// BuildProjectImage resolves these again itself, and that is the choke
// point: "never a build without a declared secret" has to be a property
// of the function that builds. This is for the caller that can refuse
// sooner and better - a missing variable is the project's own declaration,
// so it is worth saying before a container runtime is even looked for.
func ValidateBuildSecrets(names []string) error {
	_, err := resolveBuildSecrets(names)
	return err
}

// buildSecretFlags is the docker build arguments for resolved secret
// names: one `--secret id=NAME,env=NAME` each, which tells BuildKit to
// read that variable from inspector's environment and expose it only to a
// RUN step that mounts it (`RUN --mount=type=secret,id=NAME`). Unlike a
// --build-arg, nothing about it is recorded in the image, its history or
// its layers.
func buildSecretFlags(names []string) []string {
	flags := make([]string, 0, 2*len(names))
	for _, name := range names {
		flags = append(flags, "--secret", "id="+name+",env="+name)
	}
	return flags
}
