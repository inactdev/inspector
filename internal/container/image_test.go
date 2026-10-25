package container

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTestBase is the base image the build tests below build on top of:
// small, already needed by the rest of this package's docker tests, and
// carrying a shell so a built image can actually run something.
const buildTestBase = "alpine"

func requireDockerForBuild(t *testing.T) {
	t.Helper()
	if err := EnsureAvailable(); err != nil {
		t.Skipf("no usable container runtime, skipping: %v", err)
	}
}

func writeDockerfile(t *testing.T, content string) (dockerfile, contextDir string) {
	t.Helper()
	contextDir = t.TempDir()
	dockerfile = filepath.Join(contextDir, "Dockerfile.inspector")
	if err := os.WriteFile(dockerfile, []byte(content), 0o644); err != nil {
		t.Fatalf("writing the Dockerfile: %v", err)
	}
	return dockerfile, contextDir
}

func TestParseDockerfile_CollectsOutsideBasesOnly(t *testing.T) {
	facts, err := parseDockerfile(`
# a comment
FROM --platform=linux/amd64 golang:1.22 AS build
RUN go build ./...
FROM scratch AS empty
FROM build
FROM alpine:3.20
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"golang:1.22", "alpine:3.20"}
	if strings.Join(facts.baseRefs, ",") != strings.Join(want, ",") {
		t.Fatalf("baseRefs = %v, want %v - scratch and an earlier stage pull in no outside image", facts.baseRefs, want)
	}
	if facts.readsBuildContext {
		t.Fatal("readsBuildContext = true, want false: nothing here copies from the context")
	}
}

func TestParseDockerfile_BuildContextDetection(t *testing.T) {
	// Which of these read the build context decides whether an
	// already-tagged image may be reused without building: the tag
	// covers the Dockerfile and its bases, never the context, so a
	// Dockerfile that bakes dependencies in has to be rebuilt every run
	// or a changed lockfile would silently keep yesterday's install.
	cases := map[string]struct {
		dockerfile string
		want       bool
	}{
		"toolchain only":      {"FROM alpine\nRUN apk add --no-cache git\n", false},
		"copy from context":   {"FROM alpine\nCOPY package-lock.json ./\n", true},
		"add from context":    {"FROM alpine\nADD src.tar.gz /src\n", true},
		"copy with chown":     {"FROM alpine\nCOPY --chown=1000:1000 . /app\n", true},
		"copy from stage":     {"FROM alpine AS build\nFROM alpine\nCOPY --from=build /out /out\n", false},
		"copy from image":     {"FROM alpine\nCOPY --from=busybox:latest /bin/busybox /bin/\n", false},
		"run with mount":      {"FROM alpine\nRUN --mount=type=bind,target=/src true\n", true},
		"run mentioning from": {"FROM alpine\nRUN echo --from=nowhere > /dev/null\n", false},
		"continued copy": {"FROM alpine\nCOPY \\\n" +
			"# a comment inside the continuation\n" +
			"  go.mod go.sum ./\n", true},
	}
	for name, tc := range cases {
		facts, err := parseDockerfile(tc.dockerfile)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if facts.readsBuildContext != tc.want {
			t.Fatalf("%s: readsBuildContext = %v, want %v", name, facts.readsBuildContext, tc.want)
		}
	}
}

func TestParseDockerfile_RefusesWhatItCannotResolve(t *testing.T) {
	// Both of these fail closed by name rather than being guessed at: a
	// variable base cannot be resolved to the digest the tag is derived
	// from, so the tag would stop changing when the base did, and a
	// custom escape character would mean this parser is reading a
	// different file than docker builds.
	cases := map[string]struct {
		dockerfile string
		want       string
	}{
		"variable base":    {"ARG V=1.22\nFROM golang:${V}\n", "variable"},
		"escape directive": {"# escape=`\nFROM alpine\n", "escape character"},
		"no FROM":          {"RUN true\n", "no FROM"},
	}
	for name, tc := range cases {
		_, err := parseDockerfile(tc.dockerfile)
		if err == nil {
			t.Fatalf("%s: expected a refusal, got none", name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %q, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestProjectImageTag_DerivedFromDockerfileAndBases(t *testing.T) {
	dockerfile := []byte("FROM alpine\n")
	base := BaseImage{Ref: "alpine", ID: "sha256:" + strings.Repeat("a", 64)}
	moved := BaseImage{Ref: "alpine", ID: "sha256:" + strings.Repeat("b", 64)}

	tag := ProjectImageTag(dockerfile, []BaseImage{base})
	if !strings.HasPrefix(tag, ProjectImageRepository+":") {
		t.Fatalf("tag = %q, want it under the %s repository", tag, ProjectImageRepository)
	}
	if tag != ProjectImageTag(dockerfile, []BaseImage{base}) {
		t.Fatal("the same Dockerfile and base produced two different tags; the cached image could never be reused")
	}
	if tag == ProjectImageTag([]byte("FROM alpine\nRUN true\n"), []BaseImage{base}) {
		t.Fatal("a changed Dockerfile produced the same tag; a stale image would be reused")
	}
	if tag == ProjectImageTag(dockerfile, []BaseImage{moved}) {
		t.Fatal("a moved base image produced the same tag; a stale image would be reused")
	}
}

func TestBuildProjectImage_BuildsThenReusesTheTag(t *testing.T) {
	requireDockerForBuild(t)
	dockerfile, contextDir := writeDockerfile(t, "FROM "+buildTestBase+"\nRUN mkdir -p /opt/inspector-build-marker\n")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var log bytes.Buffer
	image, err := BuildProjectImage(ctx, dockerfile, contextDir, &log)
	if err != nil {
		t.Fatalf("building the project image: %v\n%s", err, log.String())
	}
	t.Cleanup(func() {
		_ = removeImage(image.Tag)
	})
	if !image.Built {
		t.Fatal("Built = false on the first build of this Dockerfile")
	}
	if len(image.Bases) != 1 || image.Bases[0].Ref != buildTestBase {
		t.Fatalf("Bases = %+v, want the one base this Dockerfile names", image.Bases)
	}
	if _, err := imageID(ctx, image.Tag); err != nil {
		t.Fatalf("the built tag is not on this machine: %v", err)
	}

	// Nothing here reads the build context, so the second call must
	// reuse the tag rather than build again.
	again, err := BuildProjectImage(ctx, dockerfile, contextDir, &log)
	if err != nil {
		t.Fatalf("resolving the project image a second time: %v", err)
	}
	if again.Tag != image.Tag {
		t.Fatalf("tag = %q the second time, want the same %q", again.Tag, image.Tag)
	}
	if again.Built {
		t.Fatal("Built = true on the second call; an unchanged Dockerfile with no build context should reuse its tag")
	}
}

func TestBuildProjectImage_RebuildsWhenTheDockerfileReadsTheContext(t *testing.T) {
	requireDockerForBuild(t)
	// The tag cannot see the context, so this is the case where skipping
	// the build would bake in whatever the context held the first time
	// and never notice it changed.
	dockerfile, contextDir := writeDockerfile(t, "FROM "+buildTestBase+"\nCOPY dependency.txt /dependency.txt\n")
	if err := os.WriteFile(filepath.Join(contextDir, "dependency.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatalf("writing the context file: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var log bytes.Buffer
	image, err := BuildProjectImage(ctx, dockerfile, contextDir, &log)
	if err != nil {
		t.Fatalf("building the project image: %v\n%s", err, log.String())
	}
	t.Cleanup(func() {
		_ = removeImage(image.Tag)
	})

	if err := os.WriteFile(filepath.Join(contextDir, "dependency.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatalf("rewriting the context file: %v", err)
	}
	again, err := BuildProjectImage(ctx, dockerfile, contextDir, &log)
	if err != nil {
		t.Fatalf("rebuilding the project image: %v", err)
	}
	if !again.Built {
		t.Fatal("Built = false after the build context changed; the image would hold the old content under the same tag")
	}

	out, err := runImage(ctx, again.Tag, "cat /dependency.txt")
	if err != nil {
		t.Fatalf("running the rebuilt image: %v\n%s", err, out)
	}
	if !strings.Contains(out, "second") {
		t.Fatalf("the rebuilt image holds %q, want the context's current content", out)
	}
}

func TestBuildProjectImage_FailedBuildIsRefusedWithItsLog(t *testing.T) {
	requireDockerForBuild(t)
	dockerfile, contextDir := writeDockerfile(t, "FROM "+buildTestBase+"\nRUN echo this-build-fails-on-purpose >&2; exit 3\n")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var log bytes.Buffer
	image, err := BuildProjectImage(ctx, dockerfile, contextDir, &log)
	if !errors.Is(err, ErrProjectImageBuildFailed) {
		t.Fatalf("err = %v, want it to wrap ErrProjectImageBuildFailed", err)
	}
	if !strings.Contains(image.Log, "this-build-fails-on-purpose") {
		t.Fatalf("Log = %q, want docker's own output kept for the refusal", image.Log)
	}
	if !strings.Contains(log.String(), "this-build-fails-on-purpose") {
		t.Fatalf("live log = %q, want docker's output streamed as it happened", log.String())
	}
}

func TestBuildProjectImage_UnresolvableBaseIsRefused(t *testing.T) {
	requireDockerForBuild(t)
	// Unreachable whether or not this machine has a registry it can
	// talk to, so the refusal is the same either way.
	dockerfile, contextDir := writeDockerfile(t, "FROM inspector-test-base-does-not-exist-xyz:0\n")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_, err := BuildProjectImage(ctx, dockerfile, contextDir, &bytes.Buffer{})
	if !errors.Is(err, ErrProjectImageUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrProjectImageUnavailable", err)
	}
}

func TestBuildProjectImage_MissingDockerfileIsRefused(t *testing.T) {
	// No docker needed: the file is read before anything is run.
	_, err := BuildProjectImage(context.Background(), filepath.Join(t.TempDir(), "Dockerfile.inspector"), t.TempDir(), &bytes.Buffer{})
	if !errors.Is(err, ErrProjectImageUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrProjectImageUnavailable", err)
	}
}

// removeImage drops a tag a build test created, so a suite run does not
// leave one behind per test. The image's layers stay in docker's cache,
// which is what makes the next run fast.
func removeImage(tag string) error {
	return exec.Command("docker", "image", "rm", "--force", tag).Run()
}

// runImage runs one command in a built image, to assert what actually
// ended up inside it rather than only what docker reported building.
func runImage(ctx context.Context, tag, command string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", tag, "sh", "-c", command).CombinedOutput()
	return string(out), err
}
