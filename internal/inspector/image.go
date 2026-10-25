package inspector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

// ProjectDockerfileName is the one file a project declares its check
// environment in, at the repository root. Fixed rather than configurable,
// by ruling: the same image is meant to be the worker's box and the
// judge's box, and a configurable path would be a second way to say the
// same thing - one that inspector-gate, which protects files by name from
// the base branch alone, could not protect without the branch under test
// getting a say in where it lives.
const ProjectDockerfileName = "Dockerfile.inspector"

// ImageBuildTimeout bounds how long building that image may take before
// inspector refuses rather than hanging. Deliberately not the project's
// own timeoutSeconds, which bounds the check command: a first build that
// installs a project's whole dependency tree can legitimately take far
// longer than the check it then runs, and a project tightening its check
// timeout must not thereby break its own build. Not configurable, because
// nobody has needed it to be; it exists so an unattended Fabrica handoff
// cannot hang forever.
const ImageBuildTimeout = 30 * time.Minute

// ErrNoProjectImage means the repo commits no ProjectDockerfileName, so
// there is no image to run the check in. inspector refuses rather than
// guessing at a toolchain or falling back to one of its own: an image
// nobody declared would judge the project against an environment nobody
// asked for.
var ErrNoProjectImage = errors.New("no " + ProjectDockerfileName + " committed")

// ErrUncommittedProjectImage means ProjectDockerfileName exists on disk
// but git does not track it - in practice, a gitignored one, since an
// ordinary untracked file already fails the clean-tree check. It is
// refused for the same reason the working tree has to be clean: an image
// built from a file that is not in the commit cannot be bound to that
// commit, and nobody reviewing the pull request would ever see it.
var ErrUncommittedProjectImage = errors.New(ProjectDockerfileName + " is not committed")

// ProjectDockerfile returns the path of repoRoot's committed
// ProjectDockerfileName, or ErrNoProjectImage / ErrUncommittedProjectImage.
func ProjectDockerfile(repoRoot string) (string, error) {
	path := filepath.Join(repoRoot, ProjectDockerfileName)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoProjectImage
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a Dockerfile", path)
	}

	tracked, err := runValidationGit(repoRoot, "ls-files", "--full-name", "-z", "--", ProjectDockerfileName)
	if err != nil {
		return "", fmt.Errorf("checking whether %s is committed: %w", ProjectDockerfileName, err)
	}
	found := false
	for _, name := range splitNullPaths(tracked) {
		if name == ProjectDockerfileName {
			found = true
		}
	}
	if !found {
		return "", ErrUncommittedProjectImage
	}
	return path, nil
}

// BuildProjectImage resolves the image this repo's check command runs in,
// building dockerfile - which ProjectDockerfile resolved - with the
// repository root as the build context and streaming docker's output to
// log as it happens.
//
// Errors wrap container.ErrProjectImageUnavailable or
// container.ErrProjectImageBuildFailed. Either is a refusal for the
// caller: a project whose declared environment inspector could not
// produce has not been judged, and running the check in anything else
// would judge a different project.
func BuildProjectImage(repoRoot, dockerfile string, log io.Writer) (container.ProjectImage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ImageBuildTimeout)
	defer cancel()
	return container.BuildProjectImage(ctx, dockerfile, repoRoot, log)
}

// DescribeProjectImage is the one line a run prints about the image it is
// about to use, so the report and the console both say which image judged
// the code and where it came from - including which base it was resolved
// against, since that is half of what the tag is derived from.
func DescribeProjectImage(image container.ProjectImage) string {
	action := "reusing"
	if image.Built {
		action = "built"
	}
	bases := make([]string, 0, len(image.Bases))
	for _, base := range image.Bases {
		bases = append(bases, fmt.Sprintf("%s = %s", base.Ref, base.ID))
	}
	from := "no outside base image"
	if len(bases) > 0 {
		from = strings.Join(bases, ", ")
	}
	return fmt.Sprintf("inspector: %s check image %s from %s (base: %s)", action, image.Tag, ProjectDockerfileName, from)
}
