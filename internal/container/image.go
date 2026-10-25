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
// with contextDir as the build context and streaming docker's output to
// log as it happens.
//
// It is tagged by the Dockerfile's content together with the image ID
// every base it names resolves to, so the same inputs reuse the same
// local image and a changed Dockerfile or a changed base produces a
// different one. A base image that is not on this machine yet is pulled
// once: this step may use the network, which the check container itself
// never may.
//
// An already-present tag is reused without building, except when the
// Dockerfile reads the build context (any COPY or ADD that is not
// --from=<stage>, or any RUN --mount). Those make the image depend on
// repository content the tag cannot see - the whole point of baking
// dependencies in is `COPY package-lock.json` - so the build runs every
// time and docker's own layer cache is what keeps an unchanged rebuild
// cheap. See notes/20261025-project-image-from-committed-dockerfile.md.
//
// Errors wrap ErrProjectImageUnavailable or, for a build that ran and
// failed, ErrProjectImageBuildFailed; callers turn either into a refusal.
func BuildProjectImage(ctx context.Context, dockerfilePath, contextDir string, log io.Writer) (ProjectImage, error) {
	if log == nil {
		// Unlike os/exec, which reads a nil writer as /dev/null, this
		// function writes to log itself.
		log = io.Discard
	}
	content, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return ProjectImage{}, fmt.Errorf("%w: reading %s: %v", ErrProjectImageUnavailable, dockerfilePath, err)
	}
	parsed, err := parseDockerfile(string(content))
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
		Tag:        ProjectImageTag(content, bases),
		Dockerfile: dockerfilePath,
		Bases:      bases,
	}
	if !parsed.readsBuildContext {
		if _, err := imageID(ctx, image.Tag); err == nil {
			return image, nil
		}
	}

	captured := &strings.Builder{}
	out := io.MultiWriter(log, captured)
	// -f names the Dockerfile; the final argument is the build context,
	// which is the repository root. No --pull: the bases were already
	// resolved above, and re-resolving them here would let a floating
	// tag move between that resolution and this build, which is the one
	// thing the content-derived tag exists to rule out. No --network
	// either: the build may reach the network to install the project's
	// dependencies, which is what makes the check container's own
	// --network none possible at all.
	cmd := exec.CommandContext(ctx, "docker", "build", "-f", dockerfilePath, "-t", image.Tag, contextDir)
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
		return image, fmt.Errorf("%w: docker build -f %s %s: %v", ErrProjectImageBuildFailed, dockerfilePath, contextDir, runErr)
	}
	return image, nil
}

// ProjectImageTag is the local tag a Dockerfile and its resolved bases
// produce. Exported so a caller can state the tag it is about to run, and
// so the derivation is testable without docker.
func ProjectImageTag(dockerfile []byte, bases []BaseImage) string {
	h := sha256.New()
	// Length-prefixed, so no arrangement of Dockerfile bytes and base
	// references can hash the same as a different one.
	fmt.Fprintf(h, "inspector project check image v1\ndockerfile %d\n", len(dockerfile))
	h.Write(dockerfile)
	for _, base := range bases {
		fmt.Fprintf(h, "\nbase %s=%s", base.Ref, base.ID)
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
// result depends on the build context as well as on the file.
type dockerfileFacts struct {
	baseRefs          []string
	readsBuildContext bool
}

// parseDockerfile reads those two facts, and refuses rather than guessing
// at anything it cannot read with certainty.
//
// It is not a general Dockerfile parser and does not need to be: docker
// itself is what builds the file, and everything here either feeds the
// content-derived tag or is refused by name. The two refusals matter more
// than the parsing:
//
//   - A `FROM` naming its image through a variable cannot be resolved to a
//     digest, so the tag would stop changing when that base changed. The
//     project is told to pin it literally instead.
//   - An `# escape=` parser directive changes which character continues a
//     line, so this function would be reading a different file than docker
//     does. Refused rather than parsed under the wrong rules.
func parseDockerfile(content string) (dockerfileFacts, error) {
	var facts dockerfileFacts
	stages := map[string]bool{}
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
			if strings.Contains(ref, "$") {
				return facts, fmt.Errorf("FROM %s names its base image through a variable; inspector tags the image it builds by that base's digest and cannot resolve one it has to expand first - name the base literally, ideally pinned by digest", ref)
			}
			facts.baseRefs = append(facts.baseRefs, ref)
		case "COPY", "ADD":
			// --from=<stage|image> copies from somewhere other than the
			// build context, so it is the file's own content, which the
			// tag already hashes. Anything else reads the context.
			if !hasFlag(args, "--from=") {
				facts.readsBuildContext = true
			}
		case "RUN":
			// A RUN --mount can bind the build context in (and
			// type=cache/secret/ssh bring in state of their own).
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

func hasFlag(args []string, prefix string) bool {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			// Flags only precede an instruction's own arguments; past
			// the first of those, a matching string is just text.
			return false
		}
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
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
