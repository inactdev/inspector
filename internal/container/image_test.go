package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// writeDockerfile puts content in a Dockerfile of its own main build
// context and returns both, plus a separate directory standing in for the
// repository under check - the second, named context every build carries.
// Separate on purpose: a test that copies from the repo context proves
// nothing if the same file is also in the main one.
func writeDockerfile(t *testing.T, content string) (dockerfile, contextDir, repoDir string) {
	t.Helper()
	contextDir = t.TempDir()
	dockerfile = filepath.Join(contextDir, "Dockerfile.inspector")
	if err := os.WriteFile(dockerfile, []byte(content), 0o644); err != nil {
		t.Fatalf("writing the Dockerfile: %v", err)
	}
	return dockerfile, contextDir, t.TempDir()
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
		// A secret mount is a RUN --mount like any other, which is why
		// issue #86 needed no new rebuild rule: a Dockerfile installing
		// private dependencies is rebuilt every run already.
		"run mounting a secret": {"FROM alpine\nRUN --mount=type=secret,id=BUNDLE_TOKEN cat /run/secrets/BUNDLE_TOKEN\n", true},
		"continued copy": {"FROM alpine\nCOPY \\\n" +
			"# a comment inside the continuation\n" +
			"  go.mod go.sum ./\n", true},
		// The repository under check is a build context too, and the tag
		// can see it no better than it sees the main one - so a project
		// baking its dependencies in from the repository has to rebuild
		// for exactly the same reason (issue #81).
		"copy from the repo context": {"FROM alpine\nCOPY --from=" + RepoBuildContext + " Gemfile Gemfile.lock /deps/\n", true},
		"add from the repo context":  {"FROM alpine\nADD --from=" + RepoBuildContext + " src.tar.gz /src\n", true},
		"run mounting the repo context": {"FROM alpine\nRUN --mount=from=" + RepoBuildContext +
			",target=/repo cp /repo/Gemfile.lock /deps/\n", true},
		"from the repo context": {"FROM " + RepoBuildContext + "\n", true},
		// Docker folds stage names to lower case and resolves --from=
		// against its named contexts first, so this reaches the same
		// directory the lower-case spelling does.
		"copy from the repo context in another case": {"FROM alpine\nCOPY --from=REPO Gemfile /deps/\n", true},
		// Two --from= flags on one line is not a Dockerfile docker
		// builds, but whichever of them a builder were to take, the one
		// naming the repository has to count: a needless rebuild costs a
		// cached layer, a missed one costs yesterday's dependencies.
		"copy from a stage and the repo context": {"FROM alpine AS build\nFROM alpine\n" +
			"COPY --from=build --from=" + RepoBuildContext + " Gemfile /deps/\n", true},
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

// A stage named after the repository build context is refused rather than
// built: docker resolves the context ahead of a stage of the same name, so
// the project's own stage would be silently replaced by the repository
// under check - and whatever that stage was meant to contribute would
// vanish from the image the check then runs in.
func TestParseDockerfile_RefusesAStageNamedAfterTheRepoContext(t *testing.T) {
	for name, offending := range map[string]string{
		"lower case": "FROM alpine AS " + RepoBuildContext,
		// Refused in any case, because docker folds stage names.
		"mixed case":                    "FROM alpine AS Repo",
		"platform flag before the base": "FROM --platform=linux/amd64 alpine AS " + RepoBuildContext,
	} {
		_, err := parseDockerfile("FROM alpine AS first\n" + offending + "\nCOPY . /src\n")
		if err == nil {
			t.Fatalf("%s: expected a refusal, got none", name)
		}
		if !strings.Contains(err.Error(), offending) {
			t.Fatalf("%s: error = %q, want it to name the offending line %q", name, err, offending)
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
	dockerfile, contextDir, repoDir := writeDockerfile(t, "FROM "+buildTestBase+"\nRUN mkdir -p /opt/inspector-build-marker\n")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var log bytes.Buffer
	image, err := BuildProjectImage(ctx, dockerfile, contextDir, repoDir, nil, &log)
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
	again, err := BuildProjectImage(ctx, dockerfile, contextDir, repoDir, nil, &log)
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
	dockerfile, contextDir, repoDir := writeDockerfile(t, "FROM "+buildTestBase+"\nCOPY dependency.txt /dependency.txt\n")
	if err := os.WriteFile(filepath.Join(contextDir, "dependency.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatalf("writing the context file: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var log bytes.Buffer
	image, err := BuildProjectImage(ctx, dockerfile, contextDir, repoDir, nil, &log)
	if err != nil {
		t.Fatalf("building the project image: %v\n%s", err, log.String())
	}
	t.Cleanup(func() {
		_ = removeImage(image.Tag)
	})

	if err := os.WriteFile(filepath.Join(contextDir, "dependency.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatalf("rewriting the context file: %v", err)
	}
	again, err := BuildProjectImage(ctx, dockerfile, contextDir, repoDir, nil, &log)
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

// The repository under check is a second build context under a fixed
// name, so a Dockerfile that lives somewhere else entirely - a terms
// folder - can still bake that repository's declared dependencies into
// the image the check runs in (issue #81). The main context here does not
// hold the file at all, so only the named context can satisfy the COPY.
func TestBuildProjectImage_CopiesFromTheRepoContext(t *testing.T) {
	requireDockerForBuild(t)
	dockerfile, contextDir, repoDir := writeDockerfile(t, "FROM "+buildTestBase+
		"\nCOPY --from="+RepoBuildContext+" Gemfile.lock /deps/Gemfile.lock\n")
	lockfile := filepath.Join(repoDir, "Gemfile.lock")
	if err := os.WriteFile(lockfile, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("writing the repository's lockfile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(contextDir, "Gemfile.lock")); !os.IsNotExist(err) {
		t.Fatalf("the main build context holds a Gemfile.lock too (%v), so this test would pass without the named context", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var log bytes.Buffer
	image, err := BuildProjectImage(ctx, dockerfile, contextDir, repoDir, nil, &log)
	if err != nil {
		t.Fatalf("building the project image: %v\n%s", err, log.String())
	}
	t.Cleanup(func() {
		_ = removeImage(image.Tag)
	})
	out, err := runImage(ctx, image.Tag, "cat /deps/Gemfile.lock")
	if err != nil {
		t.Fatalf("running the built image: %v\n%s", err, out)
	}
	if !strings.Contains(out, "first") {
		t.Fatalf("the image holds %q, want the repository's own lockfile content", out)
	}

	// And the same reason the main context forces a rebuild applies here:
	// the tag cannot see the repository either, so a changed lockfile must
	// not keep yesterday's dependencies.
	if err := os.WriteFile(lockfile, []byte("second\n"), 0o644); err != nil {
		t.Fatalf("rewriting the repository's lockfile: %v", err)
	}
	again, err := BuildProjectImage(ctx, dockerfile, contextDir, repoDir, nil, &log)
	if err != nil {
		t.Fatalf("rebuilding the project image: %v", err)
	}
	if !again.Built {
		t.Fatal("Built = false after the repository's lockfile changed; the image would hold the old dependencies under the same tag")
	}
	out, err = runImage(ctx, again.Tag, "cat /deps/Gemfile.lock")
	if err != nil {
		t.Fatalf("running the rebuilt image: %v\n%s", err, out)
	}
	if !strings.Contains(out, "second") {
		t.Fatalf("the rebuilt image holds %q, want the repository's current lockfile content", out)
	}
}

// Every build carries the repository context, so a caller that names no
// repository is refused rather than having the flag quietly dropped - a
// build missing a context the Dockerfile copies from fails for a reason
// that would read as the project's own.
func TestBuildProjectImage_MissingRepoContextIsRefused(t *testing.T) {
	// No docker needed: nothing is run without a repository to name.
	dockerfile, contextDir, _ := writeDockerfile(t, "FROM "+buildTestBase+"\n")
	_, err := BuildProjectImage(context.Background(), dockerfile, contextDir, "", nil, &bytes.Buffer{})
	if !errors.Is(err, ErrProjectImageUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrProjectImageUnavailable", err)
	}
	if !strings.Contains(err.Error(), RepoBuildContext) {
		t.Fatalf("err = %q, want it to name the context that was missing", err)
	}
}

func TestBuildProjectImage_FailedBuildIsRefusedWithItsLog(t *testing.T) {
	requireDockerForBuild(t)
	dockerfile, contextDir, repoDir := writeDockerfile(t, "FROM "+buildTestBase+"\nRUN echo this-build-fails-on-purpose >&2; exit 3\n")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var log bytes.Buffer
	image, err := BuildProjectImage(ctx, dockerfile, contextDir, repoDir, nil, &log)
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
	dockerfile, contextDir, repoDir := writeDockerfile(t, "FROM inspector-test-base-does-not-exist-xyz:0\n")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_, err := BuildProjectImage(ctx, dockerfile, contextDir, repoDir, nil, &bytes.Buffer{})
	if !errors.Is(err, ErrProjectImageUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrProjectImageUnavailable", err)
	}
}

func TestBuildProjectImage_MissingDockerfileIsRefused(t *testing.T) {
	// No docker needed: the file is read before anything is run.
	_, err := BuildProjectImage(context.Background(), filepath.Join(t.TempDir(), "Dockerfile.inspector"), t.TempDir(), t.TempDir(), nil, &bytes.Buffer{})
	if !errors.Is(err, ErrProjectImageUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrProjectImageUnavailable", err)
	}
}

// Issue #86's requirement, both halves at once: the RUN step that mounts
// a declared build secret sees the real value, and that value survives
// nowhere - not in the image's history, not in any layer, not in the log
// inspector keeps.
//
// The step writes the value's *length* into the image, which proves it
// read the whole thing without the proof itself being a copy of the
// secret. Nothing in the Dockerfile may name the value either, since a
// RUN command line is recorded in the image's own history.
func TestBuildProjectImage_SecretReachesTheBuildAndSurvivesNowhere(t *testing.T) {
	requireDockerForBuild(t)
	const name = "INSPECTOR_TEST_BUILD_SECRET"
	t.Setenv(name, fakeSecretValue)
	// Mounted as a file at the default /run/secrets/<id>, which is the
	// form that needs no particular Dockerfile frontend version. The tr
	// drops a trailing newline if the builder ever adds one, so this
	// measures the value rather than its framing.
	dockerfile, contextDir, repoDir := writeDockerfile(t, "FROM "+buildTestBase+"\n"+
		"RUN --mount=type=secret,id="+name+" tr -d '\\n' < /run/secrets/"+name+
		" | wc -c | tr -d ' \\n' > /secret-length\n")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var log bytes.Buffer
	image, err := BuildProjectImage(ctx, dockerfile, contextDir, repoDir, []string{name}, &log)
	if err != nil {
		t.Fatalf("building with a declared build secret: %v\n%s", err, log.String())
	}
	t.Cleanup(func() {
		_ = removeImage(image.Tag)
	})
	if strings.Join(image.SecretNames, ",") != name {
		t.Fatalf("SecretNames = %v, want the one declared name - a run has to be able to say which secrets it handed the build", image.SecretNames)
	}

	out, err := runImage(ctx, image.Tag, "cat /secret-length")
	if err != nil {
		t.Fatalf("reading what the build measured: %v\n%s", err, out)
	}
	want := strconv.Itoa(len(fakeSecretValue))
	if strings.TrimSpace(out) != want {
		t.Fatalf("the mounting RUN step measured %q, want %q - it did not see the secret's value", strings.TrimSpace(out), want)
	}

	// Now the leak hunt, over every place the value could have been
	// recorded. docker history carries every instruction and every
	// build argument an image was made with - which is exactly why a
	// secret is not passed as a --build-arg.
	history, err := exec.CommandContext(ctx, "docker", "history", "--no-trunc", image.Tag).CombinedOutput()
	if err != nil {
		t.Fatalf("reading the image's history: %v\n%s", err, history)
	}
	if bytes.Contains(history, []byte(fakeSecretValue)) {
		t.Fatal("the secret's value is in docker history for the check image")
	}
	if strings.Contains(image.Log, fakeSecretValue) {
		t.Fatal("the secret's value is in the build log inspector keeps and can write under .inspector/")
	}
	if strings.Contains(log.String(), fakeSecretValue) {
		t.Fatal("the secret's value is in the build output inspector streams to its caller")
	}

	// And every layer's files, config and manifest: docker save writes
	// all of them into one archive, so one scan covers the lot.
	archive := filepath.Join(t.TempDir(), "image.tar")
	if out, err := exec.CommandContext(ctx, "docker", "save", "--output", archive, image.Tag).CombinedOutput(); err != nil {
		t.Fatalf("saving the image to search its layers: %v\n%s", err, out)
	}
	if fileHoldsBytes(t, archive, fakeSecretValue) {
		t.Fatal("the secret's value is in one of the check image's own layers")
	}
}

// A secret inspector's environment does not carry refuses the run by that
// variable's name, with nothing built - not even a docker command run, so
// this needs no daemon.
func TestBuildProjectImage_UnsetSecretIsRefusedWithoutBuilding(t *testing.T) {
	const name = "INSPECTOR_TEST_BUILD_SECRET_ABSENT"
	t.Setenv(name, "")
	os.Unsetenv(name)
	// A Dockerfile that would fail loudly if it were ever built, so a
	// build that started anyway cannot pass this test quietly.
	dockerfile, contextDir, repoDir := writeDockerfile(t, "FROM "+buildTestBase+"\nRUN exit 9\n")

	var log bytes.Buffer
	image, err := BuildProjectImage(context.Background(), dockerfile, contextDir, repoDir, []string{name}, &log)
	if !errors.Is(err, ErrBuildSecretUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrBuildSecretUnavailable", err)
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("err = %q, want it to name the variable to set", err)
	}
	if image.Built || image.Tag != "" || log.Len() != 0 {
		t.Fatalf("image = %+v, log = %q: nothing may be built, tagged or logged for a build that was refused", image, log.String())
	}
}

// fileHoldsBytes reports whether path contains needle anywhere, reading in
// chunks with an overlap so a match split across two reads is still found.
// Chunked rather than read whole, because an image archive is as large as
// the project's own base image.
func fileHoldsBytes(t *testing.T, path, needle string) bool {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer file.Close()

	target := []byte(needle)
	overlap := len(target) - 1
	buf := make([]byte, 1<<20+overlap)
	kept := 0
	for {
		n, err := file.Read(buf[kept:])
		if n > 0 {
			window := buf[:kept+n]
			if bytes.Contains(window, target) {
				return true
			}
			kept = min(len(window), overlap)
			copy(buf, window[len(window)-kept:])
		}
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
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
