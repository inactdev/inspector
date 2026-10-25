package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/documenter"
	"github.com/inactdev/inspector/internal/guidebook"
)

func withGuidebookRun(t *testing.T, result guidebook.Result, document string, runErr error) {
	t.Helper()
	previous := generateGuidebook
	generateGuidebook = func(context.Context, guidebook.AgentOptions) (guidebook.Result, []byte, error) {
		return result, []byte(document), runErr
	}
	t.Cleanup(func() { generateGuidebook = previous })
}

func withFactsRun(t *testing.T, facts guidebook.Facts, prepareErr error) {
	t.Helper()
	previous := prepareGuidebook
	prepareGuidebook = func(string) (*documenter.Corpus, guidebook.Facts, error) {
		return nil, facts, prepareErr
	}
	t.Cleanup(func() { prepareGuidebook = previous })
}

// generatedResult is a complete generation, as RunAgent returns one.
func generatedResult(root string) guidebook.Result {
	return guidebook.Result{
		Kind:     guidebook.Generated,
		Project:  "orchard",
		Commit:   "abc123",
		Model:    "test-model",
		Features: []string{"Baskets"},
		Facts:    12,
		FactsBy:  map[string]int{"http route": 4},
		Corpus:   documenter.CorpusSummary{Root: root, Files: 9},
		Lookups:  3,
	}
}

func TestGuidebook_WritesTheGuidebookAndSaysItIsUnconfirmed(t *testing.T) {
	root := t.TempDir()
	withGuidebookRun(t, generatedResult(root), "---\nconfirmed: no\n---\n", nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"guidebook", "--repo", root, "--model", "test-model"}, &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit = %d, want %d\n%s", code, exitGreen, stderr.String())
	}
	written, err := os.ReadFile(filepath.Join(root, guidebook.FileName))
	if err != nil {
		t.Fatalf("reading the written guidebook: %v", err)
	}
	if string(written) != "---\nconfirmed: no\n---\n" {
		t.Errorf("written guidebook = %q", written)
	}
	if !strings.Contains(stdout.String(), "features: 1") || !strings.Contains(stdout.String(), "Baskets") {
		t.Errorf("stdout does not report the feature map:\n%s", stdout.String())
	}
	// Loud, and on stderr: a guidebook mistaken for authority is the one
	// failure this design exists to prevent.
	if !strings.Contains(stderr.String(), "UNCONFIRMED") || !strings.Contains(stderr.String(), warningBar) {
		t.Errorf("stderr does not warn that the guidebook is unconfirmed:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"vendor": "anthropic (default, no flag given)"`) {
		t.Errorf("the report does not record the default vendor explicitly:\n%s", stdout.String())
	}
}

// TestGuidebook_SecondRunLeavesTheConfirmedFileAlone is "running it twice is
// safe" through the command.
func TestGuidebook_SecondRunLeavesTheConfirmedFileAlone(t *testing.T) {
	root := t.TempDir()
	confirmed := "---\nconfirmed: 2026-10-25 Jane Mallory\n---\nchecked by hand\n"
	if err := os.WriteFile(filepath.Join(root, guidebook.FileName), []byte(confirmed), 0o644); err != nil {
		t.Fatalf("seeding a confirmed guidebook: %v", err)
	}
	withGuidebookRun(t, generatedResult(root), "regenerated\n", nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"guidebook", "--repo", root, "--model", "test-model"}, &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit = %d, want %d\n%s", code, exitGreen, stderr.String())
	}
	if got, _ := os.ReadFile(filepath.Join(root, guidebook.FileName)); string(got) != confirmed {
		t.Fatalf("%s = %q, want it untouched", guidebook.FileName, got)
	}
	if got, _ := os.ReadFile(filepath.Join(root, guidebook.DraftFileName)); string(got) != "regenerated\n" {
		t.Fatalf("%s = %q, want the new document beside the confirmed one", guidebook.DraftFileName, got)
	}
	if !strings.Contains(stdout.String(), "left untouched") {
		t.Errorf("stdout does not say there are now two files:\n%s", stdout.String())
	}
}

func TestGuidebook_RefusesWithoutAModel(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"guidebook"}, &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "missing required --model") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestGuidebook_RefusesFreeText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"guidebook", "--model", "m", "write me a guidebook"}, &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "does not accept free text") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestGuidebook_ReportsAFailedGenerationAsARefusal(t *testing.T) {
	withGuidebookRun(t, guidebook.Result{}, "", errors.New("model returned no content"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"guidebook", "--repo", t.TempDir(), "--model", "test-model"}, &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "no guidebook was written") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestGuidebook_ReportsAnUnwritableRootAsARefusal keeps a generated document
// that reached no file from exiting as though it had.
func TestGuidebook_ReportsAnUnwritableRootAsARefusal(t *testing.T) {
	withGuidebookRun(t, generatedResult(filepath.Join(t.TempDir(), "gone")), "document\n", nil)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"guidebook", "--model", "test-model"}, &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "could not be written") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestGuidebook_FactsOnlyNeedsNoModelAndWritesNothing is the deterministic half
// on its own: the part of the output a reader is meant to check, available
// without a credential.
func TestGuidebook_FactsOnlyNeedsNoModelAndWritesNothing(t *testing.T) {
	root := t.TempDir()
	withFactsRun(t, guidebook.Facts{
		Project: "orchard",
		Commit:  "abc123",
		Scanned: 9,
		Facts: []guidebook.Fact{{
			ID: "f1", Category: guidebook.FactRoute, Detail: "GET /baskets", Path: "server.js", Line: 12,
		}},
	}, nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"guidebook", "--repo", root, "--facts-only"}, &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit = %d, want %d\n%s", code, exitGreen, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"project: orchard", "GET /baskets", "server.js:12", "nothing was written"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(root, guidebook.FileName)); !os.IsNotExist(err) {
		t.Error("--facts-only wrote a guidebook")
	}
}

func TestGuidebook_FactsOnlyReportsAProjectItCannotRead(t *testing.T) {
	withFactsRun(t, guidebook.Facts{}, errors.New("not a git repository"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"guidebook", "--facts-only"}, &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "not a git repository") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestGuidebook_HelpIsAUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"guidebook", "--help"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit = %d, want %d so a help request is never read as a verdict", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), guidebook.FileName) {
		t.Errorf("usage does not say where the guidebook goes:\n%s", stderr.String())
	}
}

// TestGuidebook_PostsNoStatus is the design: generating a guidebook is
// setup-time work a person then confirms, so it gates nothing. The command
// would fail trying to reach GitHub if it ever posted one.
func TestGuidebook_PostsNoStatus(t *testing.T) {
	root := t.TempDir()
	withGuidebookRun(t, generatedResult(root), "document\n", nil)
	t.Setenv("GITHUB_TOKEN", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"guidebook", "--repo", root, "--model", "test-model"}, &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit = %d, want %d with no token at all\n%s", code, exitGreen, stderr.String())
	}
}
