package inspector

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/container"
)

// repoContextDockerfile is the shape issue #81 exists for: a project whose
// check image bakes its own declared dependencies in, reading them from
// the repository under check by the one fixed context name - which has to
// work the same whether the Dockerfile lives in the repository or in a
// terms folder beside it.
const repoContextDockerfile = "FROM " + testImage + "\n" +
	"COPY --from=" + container.RepoBuildContext + " Gemfile.lock /deps/Gemfile.lock\n"

// The work-mode case the issue was found on: the terms, and so the main
// build context, are outside the repository entirely, and the Dockerfile
// still has to reach the repository's committed lockfile.
func TestBuildProjectImage_TermsDockerfileReadsTheRepository(t *testing.T) {
	requireDocker(t)
	repo := newTestRepo(t, map[string]string{"Gemfile.lock": "first\n"})
	runGitT(t, repo, "rm", "-q", ProjectDockerfileName)
	runGitT(t, repo, "commit", "-q", "-m", "a repository that carries no terms")
	termsDir := t.TempDir()
	writeFiles(t, termsDir, map[string]string{
		ConfigFileName:        `{"check": "true"}`,
		ProjectDockerfileName: repoContextDockerfile,
	})

	terms, err := ResolveTerms(repo, termsDir)
	if err != nil {
		t.Fatalf("resolving the terms folder: %v", err)
	}
	dockerfile, err := terms.Dockerfile()
	if err != nil {
		t.Fatalf("resolving the terms Dockerfile: %v", err)
	}

	var log bytes.Buffer
	image, err := BuildProjectImage(terms.Dir, repo, dockerfile, &log)
	if err != nil {
		t.Fatalf("building the project image: %v\n%s", err, log.String())
	}
	t.Cleanup(func() { removeImage(t, image.Tag) })
	if out := runInImage(t, image.Tag, "cat /deps/Gemfile.lock"); !strings.Contains(out, "first") {
		t.Fatalf("the image holds %q, want the repository's committed lockfile - a terms Dockerfile has no other way to bake this project's dependencies in", out)
	}

	// A new commit changing that file has to reach the image, or the
	// check would run against yesterday's dependencies while claiming to
	// judge this commit.
	writeFiles(t, repo, map[string]string{"Gemfile.lock": "second\n"})
	runGitT(t, repo, "commit", "-q", "-am", "bump a dependency")

	again, err := BuildProjectImage(terms.Dir, repo, dockerfile, &log)
	if err != nil {
		t.Fatalf("rebuilding the project image: %v\n%s", err, log.String())
	}
	t.Cleanup(func() { removeImage(t, again.Tag) })
	if !again.Built {
		t.Fatal("Built = false after a commit changed the lockfile; the tag cannot see the repository, so this image would be the old one")
	}
	if out := runInImage(t, again.Tag, "cat /deps/Gemfile.lock"); !strings.Contains(out, "second") {
		t.Fatalf("the rebuilt image holds %q, want the lockfile as this commit has it", out)
	}
}

// And the same Dockerfile works unchanged for a project that commits it,
// where the repository is both contexts - which is what lets Fabrica's
// register write one Dockerfile for either mode.
func TestBuildProjectImage_InRepoDockerfileReadsTheRepositoryByName(t *testing.T) {
	requireDocker(t)
	repo := newTestRepo(t, map[string]string{
		ConfigFileName:        `{"check": "true"}`,
		"Gemfile.lock":        "committed\n",
		ProjectDockerfileName: repoContextDockerfile,
	})

	terms, err := ResolveTerms(repo, "")
	if err != nil {
		t.Fatalf("resolving the repository's own terms: %v", err)
	}
	dockerfile, err := terms.Dockerfile()
	if err != nil {
		t.Fatalf("resolving the committed Dockerfile: %v", err)
	}

	var log bytes.Buffer
	image, err := BuildProjectImage(terms.Dir, repo, dockerfile, &log)
	if err != nil {
		t.Fatalf("building the project image: %v\n%s", err, log.String())
	}
	t.Cleanup(func() { removeImage(t, image.Tag) })
	if out := runInImage(t, image.Tag, "cat /deps/Gemfile.lock"); !strings.Contains(out, "committed") {
		t.Fatalf("the image holds %q, want the repository's committed lockfile", out)
	}
}

// A Dockerfile declaring its own stage under the context's name is
// refused, because docker would resolve the context first and silently
// replace that stage with the repository under check.
func TestBuildProjectImage_RefusesAStageNamedAfterTheRepoContext(t *testing.T) {
	// No runtime needed: the Dockerfile is read before anything is built.
	repo := newTestRepo(t, map[string]string{
		ProjectDockerfileName: "FROM " + testImage + " AS " + container.RepoBuildContext + "\n" +
			"RUN true\nFROM " + testImage + "\n",
	})

	terms, err := ResolveTerms(repo, "")
	if err != nil {
		t.Fatalf("resolving the repository's own terms: %v", err)
	}
	dockerfile, err := terms.Dockerfile()
	if err != nil {
		t.Fatalf("resolving the committed Dockerfile: %v", err)
	}

	_, err = BuildProjectImage(terms.Dir, repo, dockerfile, &bytes.Buffer{})
	if !errors.Is(err, container.ErrProjectImageUnavailable) {
		t.Fatalf("err = %v, want it to wrap container.ErrProjectImageUnavailable, which Run turns into a refusal", err)
	}
	if !strings.Contains(err.Error(), "AS "+container.RepoBuildContext) {
		t.Fatalf("err = %q, want it to name the line that declares the stage", err)
	}
}

// runInImage runs one command in a built image, to assert what actually
// ended up inside it rather than only what docker reported building.
func runInImage(t *testing.T, tag, command string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", tag, "sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run %s: %v\n%s", tag, err, out)
	}
	return string(out)
}

// removeImage drops a tag a build test created, so a suite run does not
// leave one behind per test. The layers stay in docker's cache, which is
// what makes the next run fast.
func removeImage(t *testing.T, tag string) {
	t.Helper()
	_ = exec.Command("docker", "image", "rm", "--force", tag).Run()
}
