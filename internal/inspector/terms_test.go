package inspector

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/container"
)

// Without -terms nothing changes: the terms are the repository's own, so
// there is nothing extra to mount and the Dockerfile still has to be
// committed.
func TestResolveTerms_NoFlagNamesTheRepoRoot(t *testing.T) {
	repo := newTestRepo(t, map[string]string{ConfigFileName: `{"check": "true"}`})

	terms, err := ResolveTerms(repo, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if terms.Dir != repo {
		t.Fatalf("Dir = %q, want the repo root %q", terms.Dir, repo)
	}
	if terms.Outside {
		t.Fatal("Outside = true for the repository's own terms")
	}
	if mount := terms.Mount(); mount != (container.Mount{}) {
		t.Fatalf("Mount() = %+v, want nothing mounted for in-repo terms", mount)
	}
	cfg, err := terms.Config()
	if err != nil {
		t.Fatalf("Config(): %v", err)
	}
	if cfg.Check != "true" {
		t.Fatalf("Check = %q, want the repo's own check command", cfg.Check)
	}
}

func TestResolveTerms_OutsideFolderIsMountedAtTheFixedPath(t *testing.T) {
	repo := newTestRepo(t, nil)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		ConfigFileName:        `{"check": "sh /terms/check.sh"}`,
		ProjectDockerfileName: defaultTestDockerfile,
	})

	terms, err := ResolveTerms(repo, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !terms.Outside {
		t.Fatal("Outside = false for a folder outside the repository")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving the terms folder: %v", err)
	}
	if terms.Dir != resolved {
		t.Fatalf("Dir = %q, want the physical path %q - it becomes a mount source", terms.Dir, resolved)
	}
	want := container.Mount{Source: resolved, Target: container.TermsDir}
	if got := terms.Mount(); got != want {
		t.Fatalf("Mount() = %+v, want %+v", got, want)
	}
	cfg, err := terms.Config()
	if err != nil {
		t.Fatalf("Config(): %v", err)
	}
	if cfg.Check != "sh /terms/check.sh" {
		t.Fatalf("Check = %q, want the terms folder's own check command", cfg.Check)
	}
	// The one requirement that cannot apply to a folder no commit holds.
	if _, err := terms.Dockerfile(); err != nil {
		t.Fatalf("Dockerfile(): %v - a terms folder is in no repository, so nothing can track its Dockerfile", err)
	}
}

func TestResolveTerms_RefusesAFolderInsideTheRepository(t *testing.T) {
	// These would be in-repo terms at a path inspector-gate does not
	// protect, which is the one thing SPEC.md section 8 exists to stop.
	repo := newTestRepo(t, nil)
	inside := filepath.Join(repo, "ops", "terms")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("creating the in-repo folder: %v", err)
	}

	_, err := ResolveTerms(repo, inside)
	if !errors.Is(err, ErrTermsUnavailable) {
		t.Fatalf("error = %v, want ErrTermsUnavailable", err)
	}
	if !strings.Contains(err.Error(), "inside the repository under check") {
		t.Fatalf("error = %q, want it to say the folder is inside the repository", err)
	}
	if !strings.Contains(err.Error(), ConfigFileName) {
		t.Fatalf("error = %q, want it to name what the gate protects instead", err)
	}
}

func TestResolveTerms_RefusesAFolderHoldingTheRepository(t *testing.T) {
	// Mounting an ancestor of the repository would put whatever else
	// lives beside it inside the check container.
	parent := t.TempDir()
	repo := newTestRepoIn(t, filepath.Join(parent, "project"), nil)

	_, err := ResolveTerms(repo, parent)
	if !errors.Is(err, ErrTermsUnavailable) {
		t.Fatalf("error = %v, want ErrTermsUnavailable", err)
	}
	if !strings.Contains(err.Error(), "holds the repository under check") {
		t.Fatalf("error = %q, want it to say the folder holds the repository", err)
	}
}

func TestResolveTerms_RefusesWhatIsNotAnExistingDirectory(t *testing.T) {
	repo := newTestRepo(t, nil)
	file := filepath.Join(t.TempDir(), ConfigFileName)
	writeFiles(t, filepath.Dir(file), map[string]string{ConfigFileName: "{}"})

	cases := map[string]struct {
		dir  string
		want string
	}{
		"missing":   {filepath.Join(t.TempDir(), "absent"), "does not resolve to a real, existing path"},
		"is a file": {file, "is not a directory"},
	}
	for name, tc := range cases {
		_, err := ResolveTerms(repo, tc.dir)
		if !errors.Is(err, ErrTermsUnavailable) {
			t.Fatalf("%s: error = %v, want ErrTermsUnavailable", name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %q, want it to contain %q", name, err, tc.want)
		}
	}
}
