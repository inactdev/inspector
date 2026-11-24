package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// ProjectImageRepository is the local repository name every check image
// is tagged under. The tag itself is content-derived (see ProjectImageTag),
// so two projects whose Dockerfile and bases are identical share one
// image, and a project whose Dockerfile changes gets a different one
// without anything having to be invalidated by hand.
const ProjectImageRepository = "inspector-check"

// RepoBuildContext is the name every check image build reaches the
// repository under check by: `docker build --build-context repo=<repo
// root>`, so a Dockerfile can say
//
//	COPY --from=repo Gemfile Gemfile.lock .ruby-version /deps/
//
// and bake the project's dependencies in - the one thing a check command
// legitimately needed the network for, which it no longer has.
//
// The name is fixed with no config key, exactly like WorkspaceDir and
// TermsDir: Fabrica's `register` writes Dockerfiles against this name, and
// a configurable one would be a second way to say the same thing. It is
// passed for every build, whether the terms came from a folder outside the
// repository or from the repository itself, so one Dockerfile works either
// way.
//
// Docker resolves a named context ahead of a build stage of the same name,
// so a Dockerfile declaring its own `repo` stage is refused by name rather
// than having that stage silently replaced - see parseDockerfile.
const RepoBuildContext = "repo"

// ErrProjectImageUnavailable means the project's check image could not be
// resolved at all - its Dockerfile could not be read or parsed, or a base
// image it names could not be resolved to a digest. The check never ran,
// so callers must refuse rather than report a verdict, and must never fall
// back to some other image: the check command's toolchain is the project's
// own declaration, and running it against a different one judges something
// nobody asked about.
var ErrProjectImageUnavailable = errors.New("the project's check image could not be resolved")

// ErrProjectImageBuildFailed means docker build itself returned non-zero
// on the project's own Dockerfile. It is deliberately separate from
// ErrProjectImageUnavailable: the input was readable and its bases
// resolved, so what failed is the project's own build, and the build log
// is what the person reading the refusal needs. Still a refusal, never a
// red - a project whose image does not build has not been judged.
var ErrProjectImageBuildFailed = errors.New("the project's check image did not build")

// ProjectImage is the image one check run will use, and how it was
// obtained.
type ProjectImage struct {
	// Tag is the fully qualified local tag the check container runs,
	// under ProjectImageRepository. Derived from Dockerfile content and
	// base image digests, never from the project's own naming.
	Tag string
	// Dockerfile is the path that was built.
	Dockerfile string
	// Bases names each base image the Dockerfile pulls in from outside
	// itself, with the image ID docker resolved it to - the second half
	// of what Tag hashes.
	Bases []BaseImage
	// SecretNames are the build secrets this build was handed, by name
	// (see resolveBuildSecrets). Names only, ever: the values live in
	// inspector's environment, docker reads them from there itself, and
	// nothing here holds one.
	SecretNames []string
	// BuildArgs are the build arguments this build was handed, name and
	// value both, sorted by name - the third thing Tag hashes. Values
	// unlike SecretNames: these were read out of the repository under
	// check, docker records them in the image's own metadata, and a run
	// has to be able to say which version of a toolchain judged the
	// commit.
	BuildArgs []BuildArg
	// Built reports whether docker build actually ran, as opposed to an
	// already-present tag being reused.
	Built bool
	// Log is docker's own output for this build, empty when nothing was
	// built. The caller already saw it live; this copy is what a failure
	// can be reported and recorded with.
	Log string
}

// BaseImage is one `FROM` reference and the local image ID docker
// resolved it to.
type BaseImage struct {
	Ref string
	ID  string
}

// BuildProjectImage resolves the image a check command runs in from
// dockerfilePath - the project's own committed Dockerfile - building it
// with contextDir as the build context, repoDir as a second context named
// RepoBuildContext, secretNames as named build secrets read from
// inspector's own environment, and streaming docker's output to log as it
// happens.
//
// contextDir is the directory the Dockerfile lives in, which is not always
// the repository: with terms from a folder outside it, the Dockerfile and
// its siblings are there instead. repoDir is always the repository under
// check, so a Dockerfile that has to bake the project's own dependencies
// in can reach its lockfiles by name regardless of where the terms live
// (issue #81). It is required, and named in a refusal rather than dropped:
// a build silently missing the context a Dockerfile copies from would fail
// for a reason that looks like the project's.
//
// It is tagged by the Dockerfile's content together with the image ID
// every base it names resolves to, so the same inputs reuse the same
// local image and a changed Dockerfile or a changed base produces a
// different one. A base image that is not on this machine yet is pulled
// once: this step may use the network, which the check container itself
// never may.
//
// An already-present tag is reused without building, except when the
// Dockerfile reads either context (any COPY or ADD that is not
// --from=<stage|image>, any COPY/ADD/FROM naming RepoBuildContext, or any
// RUN --mount). Those make the image depend on repository content the tag
// cannot see - the whole point of baking dependencies in is
// `COPY package-lock.json` - so the build runs every time and docker's own
// layer cache is what keeps an unchanged rebuild cheap. See
// notes/20261025-project-image-from-committed-dockerfile.md and
// notes/20261103-the-repo-named-build-context.md.
//
// secretNames are the project's declared buildSecrets: environment
// variable names, never values. Each becomes `--secret id=NAME,env=NAME`,
// so docker reads the value from inspector's own environment and exposes
// it only to a RUN step that mounts it - never to an image layer, its
// history, or this build's log. Every one of them is resolved before
// anything else here runs, so a name inspector's environment does not
// carry refuses the run with no docker command run at all, rather than
// starting a build that was always going to fail without it.
//
// buildArgs are the project's declared buildArgs, already read out of the
// commit under check by the caller (inspector.ResolveBuildArgs) and sorted
// by name. Each becomes `--build-arg NAME=value`, each is substituted into
// every FROM reference before that base is resolved - so
// `FROM ruby:${RUBY_VERSION}` is tagged against the digest of the image it
// actually names - and all of them are hashed into the tag, which is what
// makes a changed version rebuild rather than reuse. Their names and values
// are validated here again, so no unvalidated one can reach a docker flag
// whatever a caller did.
//
// Errors wrap ErrProjectImageUnavailable - ErrBuildSecretUnavailable for a
// declared secret that is not there, ErrBuildArgUnavailable for a build
// argument inspector will not pass - or, for a build that ran and failed,
// ErrProjectImageBuildFailed; callers turn any of them into a refusal.
func BuildProjectImage(ctx context.Context, dockerfilePath, contextDir, repoDir string, secretNames []string, buildArgs []BuildArg, log io.Writer) (ProjectImage, error) {
	if log == nil {
		// Unlike os/exec, which reads a nil writer as /dev/null, this
		// function writes to log itself.
		log = io.Discard
	}
	if strings.TrimSpace(repoDir) == "" {
		return ProjectImage{}, fmt.Errorf("%w: no repository was named for the %q build context, which every check image build carries", ErrProjectImageUnavailable, RepoBuildContext)
	}
	secrets, err := resolveBuildSecrets(secretNames)
	if err != nil {
		return ProjectImage{}, err
	}
	if err := ValidateBuildArgs(buildArgs); err != nil {
		return ProjectImage{}, err
	}
	content, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return ProjectImage{}, fmt.Errorf("%w: reading %s: %v", ErrProjectImageUnavailable, dockerfilePath, err)
	}
	parsed, err := parseDockerfile(string(content), buildArgs)
	if err != nil {
		return ProjectImage{}, fmt.Errorf("%w: %s: %v", ErrProjectImageUnavailable, dockerfilePath, err)
	}

	bases := make([]BaseImage, 0, len(parsed.baseRefs))
	for _, ref := range parsed.baseRefs {
		id, err := resolveBaseImage(ctx, ref, log)
		if err != nil {
			return ProjectImage{}, err
		}
		bases = append(bases, BaseImage{Ref: ref, ID: id})
	}

	image := ProjectImage{
		Tag:         ProjectImageTag(content, bases, buildArgs),
		Dockerfile:  dockerfilePath,
		Bases:       bases,
		SecretNames: secrets,
		BuildArgs:   buildArgs,
	}
	if !parsed.readsBuildContext {
		if _, err := imageID(ctx, image.Tag); err == nil {
			return image, nil
		}
	}

	captured := &strings.Builder{}
	out := io.MultiWriter(log, captured)
	repoContext := RepoBuildContext + "=" + repoDir
	cmd := exec.CommandContext(ctx, "docker", buildCommandArgs(dockerfilePath, image.Tag, repoContext, contextDir, secrets, buildArgs)...)
	// The identical writer for both, so os/exec reuses one pipe and one
	// copying goroutine rather than racing two onto the same writer.
	cmd.Stdout = out
	cmd.Stderr = out
	runErr := cmd.Run()
	image.Built = true
	image.Log = captured.String()
	if ctx.Err() != nil {
		return image, fmt.Errorf("%w: building %s did not finish: %v", ErrProjectImageBuildFailed, dockerfilePath, ctx.Err())
	}
	if runErr != nil {
		return image, fmt.Errorf("%w: docker build -f %s --build-context %s %s: %v", ErrProjectImageBuildFailed, dockerfilePath, repoContext, contextDir, runErr)
	}
	return image, nil
}

// buildCommandArgs is the whole `docker` command line one check image
// build runs, assembled in one place so what inspector hands the builder
// is readable and testable without a daemon. Named for the command rather
// than for `--build-arg`, which is the separate thing buildArgFlags
// assembles.
//
// -f names the Dockerfile; the final argument is the main build context,
// which is wherever the Dockerfile lives. --build-context adds the
// repository under check beside it under its fixed name, for the
// Dockerfile that bakes the project's dependencies in; one --secret per
// declared build secret names an environment variable docker reads itself;
// and one --build-arg per declared build argument carries a value read out
// of the commit under check. The first two need BuildKit, which is what
// `docker build` has used by default since Docker 23, and a builder
// without it fails this build loudly in docker's own words rather than
// producing an image missing those files. No --pull: the bases were
// already resolved by the caller, and re-resolving them here would let a
// floating tag move between that resolution and this build, which is the
// one thing the content-derived tag exists to rule out. No --network
// either: the build may reach the network to install the project's
// dependencies, which is what makes the check container's own
// --network none possible at all.
//
// No secret value appears here, or anywhere else inspector writes: a
// secret is passed as the name of a variable, never as a value. A build
// argument's value does appear, here and in the built image's own
// metadata, which is docker's behavior and the reason the two are separate
// config keys.
func buildCommandArgs(dockerfilePath, tag, repoContext, contextDir string, secrets []string, args []BuildArg) []string {
	command := []string{"build", "-f", dockerfilePath, "-t", tag, "--build-context", repoContext}
	command = append(command, buildSecretFlags(secrets)...)
	command = append(command, buildArgFlags(args)...)
	return append(command, contextDir)
}

// ProjectImageTag is the local tag a Dockerfile, its resolved bases and
// the build arguments it is handed produce. Exported so a caller can state
// the tag it is about to run, and so the derivation is testable without
// docker.
//
// The build arguments are in it because they are the one input that
// changes the image while changing neither the Dockerfile nor the digest
// of any base it names: `FROM ruby:${RUBY_VERSION}` with a new
// .ruby-version is a different image built from the same file. They are
// hashed as sorted NAME=value pairs - no value in the tag itself, which is
// printed in reports and console lines - and an empty list contributes
// nothing at all, so every project that declares none keeps exactly the
// tag it had before this existed.
func ProjectImageTag(dockerfile []byte, bases []BaseImage, args []BuildArg) string {
	h := sha256.New()
	// Length-prefixed, so no arrangement of Dockerfile bytes and base
	// references can hash the same as a different one.
	fmt.Fprintf(h, "inspector project check image v1\ndockerfile %d\n", len(dockerfile))
	h.Write(dockerfile)
	for _, base := range bases {
		fmt.Fprintf(h, "\nbase %s=%s", base.Ref, base.ID)
	}
	// Neither a validated name nor a validated value can contain a
	// newline, so one pair per line stays unambiguous.
	for _, arg := range args {
		fmt.Fprintf(h, "\nbuild-arg %s=%s", arg.Name, arg.Value)
	}
	return ProjectImageRepository + ":" + hex.EncodeToString(h.Sum(nil))
}

// resolveBaseImage returns the local image ID ref resolves to, pulling it
// once if this machine does not have it yet.
//
// An image already present is deliberately not re-pulled. That is what
// `docker build` itself does without --pull, so the ID hashed into the
// tag names the bytes the build will actually use; re-pulling would let a
// floating tag change the image between two runs of the same commit. A
// project that wants a base it cannot drift pins it by digest in its own
// Dockerfile.
func resolveBaseImage(ctx context.Context, ref string, log io.Writer) (string, error) {
	if id, err := imageID(ctx, ref); err == nil {
		return id, nil
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("%w: resolving base image %s: %v", ErrProjectImageUnavailable, ref, ctx.Err())
	}

	fmt.Fprintf(log, "inspector: pulling base image %s (this machine does not have it yet)\n", ref)
	pull := exec.CommandContext(ctx, "docker", "pull", ref)
	pull.Stdout = log
	pull.Stderr = log
	if err := pull.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: pulling base image %s: %v", ErrProjectImageUnavailable, ref, ctx.Err())
		}
		return "", fmt.Errorf("%w: base image %s is not on this machine and could not be pulled: %v", ErrProjectImageUnavailable, ref, err)
	}

	id, err := imageID(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("%w: base image %s was pulled but docker could not report its image ID: %v", ErrProjectImageUnavailable, ref, err)
	}
	return id, nil
}

// imageIDPattern is what a docker image ID looks like, so a daemon
// answering something unexpected cannot end up inside an image tag.
var imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// imageID reads the local image ID of ref, or fails if this machine does
// not have it. {{.Id}} rather than {{.RepoDigests}}: the image ID is the
// image's own config digest and is always present, while RepoDigests is
// empty for anything that was never pulled or pushed by digest - which
// includes every image built locally, such as the ones this file builds.
func imageID(ctx context.Context, ref string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", ref).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker image inspect %s: %v: %s", ref, err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	if !imageIDPattern.MatchString(id) {
		return "", fmt.Errorf("docker resolved %s to %q, which is not an image ID", ref, id)
	}
	return id, nil
}

// dockerfileFacts is what inspector has to know about a Dockerfile it did
// not write: which images it pulls in from outside itself, and whether its
// result depends on either build context as well as on the file.
type dockerfileFacts struct {
	baseRefs          []string
	readsBuildContext bool
}

// parseDockerfile reads those two facts, and refuses rather than guessing
// at anything it cannot read with certainty.
//
// It is not a general Dockerfile parser and does not need to be: docker
// itself is what builds the file, and everything here either feeds the
// content-derived tag or is refused by name. The three refusals matter
// more than the parsing:
//
//   - A `FROM` naming its image through a variable no declared build
//     argument expands cannot be resolved to a digest, so the tag would
//     stop changing when that base changed. The project is told to declare
//     the argument under buildArgs, or to pin the base literally. One a
//     declared build argument does expand is substituted first, so the base
//     resolved, pulled, hashed and reported is the image the line actually
//     names.
//   - An `# escape=` parser directive changes which character continues a
//     line, so this function would be reading a different file than docker
//     does. Refused rather than parsed under the wrong rules.
//   - A stage named RepoBuildContext collides with the named build context
//     every build carries, and docker resolves the context first - so the
//     stage would be silently replaced by the repository under check.
//     Refused by name, naming the line.
func parseDockerfile(content string, buildArgs []BuildArg) (dockerfileFacts, error) {
	var facts dockerfileFacts
	stages := map[string]bool{}
	values := buildArgValues(buildArgs)
	sawInstruction := false
	sawFrom := false

	for _, line := range joinContinuations(content) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "#") {
			// Parser directives are comments, and only valid before the
			// first instruction - after that they are ordinary comments
			// docker ignores, so this one does too.
			if !sawInstruction && strings.HasPrefix(strings.ToLower(strings.TrimLeft(line, "# ")), "escape=") {
				return facts, fmt.Errorf("line %q sets a custom escape character; inspector reads this file to tag the image it builds and will not read it under different line-continuation rules - remove the directive", strings.TrimSpace(line))
			}
			continue
		}

		instruction := strings.ToUpper(fields[0])
		args := fields[1:]
		sawInstruction = true

		switch instruction {
		case "FROM":
			ref, stage, err := parseFrom(args)
			if err != nil {
				return facts, err
			}
			sawFrom = true
			// Before everything below it: a reference that needs
			// expanding has to be expanded before it is compared with a
			// stage name, with the repository context, or with scratch,
			// and before it is resolved to the digest the tag hashes.
			if strings.Contains(ref, "$") {
				expanded := substituteBuildArgs(ref, values)
				if strings.Contains(expanded, "$") {
					return facts, fmt.Errorf("FROM %s names its base image through a variable inspector cannot expand; it tags the image it builds by that base's digest and cannot resolve a reference it has to expand first - declare the variable under \"buildArgs\" in this project's inspector configuration, so its value is read from the commit under check and hashed into the tag, or name the base literally, ideally pinned by digest", ref)
				}
				ref = expanded
			}
			if namesRepoContext(stage) {
				return facts, fmt.Errorf("line %q declares a build stage named %q, which every check image build already carries as a build context holding the repository under check - docker resolves that context ahead of a stage of the same name, so this stage would be silently replaced by the repository. Rename the stage", strings.TrimSpace(line), stage)
			}
			if stage != "" {
				stages[strings.ToLower(stage)] = true
			}
			// A FROM naming an earlier stage of this same file, or
			// scratch, pulls in no outside image, so there is no digest
			// to resolve and nothing to add to the tag beyond the file
			// content already being hashed.
			if ref == "scratch" || stages[strings.ToLower(ref)] {
				continue
			}
			// Nor does a FROM naming the repository context: docker
			// resolves that name to the directory this build was handed,
			// which is no image and has no digest to pull - but it is
			// repository content, which the tag cannot see.
			if namesRepoContext(ref) {
				facts.readsBuildContext = true
				continue
			}
			facts.baseRefs = append(facts.baseRefs, ref)
		case "COPY", "ADD":
			// --from=<stage|image> copies from somewhere other than a
			// build context, so it is the file's own content, which the
			// tag already hashes. --from=<RepoBuildContext> is the
			// exception: that is the repository under check, which the
			// tag can no more see than it can see the main context.
			// Anything else reads the main context.
			//
			// Every --from= on the line is looked at, not just the first:
			// a line carrying two of them is not a file docker builds, but
			// if some builder takes the last one rather than rejecting the
			// line, the one naming the repository must still count - a
			// needless rebuild is cheap, a stale image is the failure this
			// whole rule exists to prevent.
			froms := flagValues(args, "--from=")
			if len(froms) == 0 || namesRepoContextAny(froms) {
				facts.readsBuildContext = true
			}
		case "RUN":
			// A RUN --mount can bind either build context in -
			// --mount=from=<RepoBuildContext> included - and
			// type=cache/secret/ssh bring in state of their own.
			// Counted as context-reading without inspecting the mount
			// further: the consequence of being wrong is a rebuild
			// docker's layer cache makes cheap, while the consequence of
			// the opposite is a stale image.
			if hasFlag(args, "--mount") {
				facts.readsBuildContext = true
			}
		}
	}

	// A file with no FROM at all is not a Dockerfile docker can build,
	// and saying so by name here beats handing docker a file it will
	// reject for a reason the refusal would then have to quote. A file
	// whose every FROM is internal (scratch, or an earlier stage) is
	// buildable and simply contributes no base digest to the tag.
	if !sawFrom {
		return facts, fmt.Errorf("no FROM instruction, so there is no image to build from")
	}
	return facts, nil
}

// parseFrom pulls the image reference and optional stage name out of a
// FROM instruction's arguments, skipping its flags (--platform= is the
// one in use today).
func parseFrom(args []string) (ref, stage string, err error) {
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		args = args[1:]
	}
	if len(args) == 0 {
		return "", "", fmt.Errorf("a FROM instruction names no image")
	}
	ref = args[0]
	rest := args[1:]
	if len(rest) >= 2 && strings.EqualFold(rest[0], "AS") {
		stage = rest[1]
	}
	return ref, stage, nil
}

// namesRepoContext reports whether a stage name or a --from= value names
// the repository build context. Compared case-insensitively because docker
// folds stage names to lower case and resolves a --from= against the named
// contexts first, so `--from=REPO` reaches the same directory `--from=repo`
// does. Being wrong in this direction costs a rebuild docker's layer cache
// makes cheap; being wrong in the other costs a stale image.
func namesRepoContext(name string) bool {
	return strings.EqualFold(name, RepoBuildContext)
}

// namesRepoContextAny reports whether any of these --from= values names
// the repository build context.
func namesRepoContextAny(names []string) bool {
	for _, name := range names {
		if namesRepoContext(name) {
			return true
		}
	}
	return false
}

// flagValues returns whatever follows prefix in each of an instruction's
// flags that starts with it - so prefix carries its own "=" when the value
// is what the caller is after. All of them rather than the first, because
// a caller deciding whether to rebuild must not be able to miss one.
func flagValues(args []string, prefix string) []string {
	var values []string
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			// Flags only precede an instruction's own arguments; past
			// the first of those, a matching string is just text.
			break
		}
		if strings.HasPrefix(arg, prefix) {
			values = append(values, strings.TrimPrefix(arg, prefix))
		}
	}
	return values
}

func hasFlag(args []string, prefix string) bool {
	return len(flagValues(args, prefix)) > 0
}

// joinContinuations returns the Dockerfile's logical lines, joining a line
// that ends in a backslash with the one after it the way docker does, and
// dropping comment lines that appear inside such a continuation (docker
// strips those too).
func joinContinuations(content string) []string {
	var lines []string
	var pending strings.Builder
	continuing := false

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, "\r")
		if continuing && strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmed, "\\") {
			pending.WriteString(strings.TrimSuffix(trimmed, "\\"))
			pending.WriteString(" ")
			continuing = true
			continue
		}
		if continuing {
			pending.WriteString(line)
			lines = append(lines, pending.String())
			pending.Reset()
			continuing = false
			continue
		}
		lines = append(lines, line)
	}
	if pending.Len() > 0 {
		lines = append(lines, pending.String())
	}
	return lines
}
