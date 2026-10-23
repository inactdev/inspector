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
)

const documenterTestDiff = `diff --git a/src/cli/commands.go b/src/cli/commands.go
index abc1234..def5678 100644
--- a/src/cli/commands.go
+++ b/src/cli/commands.go
@@ -1,3 +1,4 @@
 package cli
 func runBuild() {}
+func runMigrate() {}
`

func writeDocumenterDiff(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "change.diff")
	if err := os.WriteFile(path, []byte(documenterTestDiff), 0o644); err != nil {
		t.Fatalf("writing diff fixture: %v", err)
	}
	return path
}

func withDocumentationRun(t *testing.T, result documenter.Result, runErr error) {
	t.Helper()
	previous := runDocumentation
	runDocumentation = func(context.Context, documenter.AgentOptions) (documenter.Result, error) {
		return result, runErr
	}
	t.Cleanup(func() { runDocumentation = previous })
}

// staleListFinding is the PR #74 shape as the command prints it: a claim in a
// file the change never touched.
var staleListFinding = documenter.Finding{
	Kind:       documenter.FalsifiedClaim,
	Path:       "AGENTS.md",
	Line:       6,
	Claim:      "There are four of them: `runBuild`, `runCheck`,\n`runDeploy`, and `runStatus`.",
	Summary:    "the change added two entry points, so this list of four is now incomplete",
	Correction: "There are six of them: `runBuild`, `runCheck`, `runDeploy`, `runMigrate`, `runRollback`, and `runStatus`.",
	InChange:   false,
}

func TestDocumenter_ReportsAFalsifiedClaimAndExitsGreen(t *testing.T) {
	withDocumentationRun(t, documenter.Result{
		Kind:            documenter.Reviewed,
		Verdict:         documenter.DocumentationFalsified,
		Findings:        []documenter.Finding{staleListFinding},
		Corpus:          documenter.CorpusSummary{Root: "/project", Files: 42},
		Lookups:         3,
		RequestProvided: true,
	}, nil)

	var stdout, stderr bytes.Buffer
	code := run([]string{"document", "--diff", writeDocumenterDiff(t), "--model", "test-model"}, &stdout, &stderr)
	if code != exitGreen {
		// SPEC.md's documentation judgment reports; it never gates a merge.
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "documentation: documentation_falsified_claims") {
		t.Fatalf("stdout = %q, want the derived verdict printed", out)
	}
	if !strings.Contains(out, "AGENTS.md:6: falsified_claim - the change added two entry points") {
		t.Fatalf("stdout = %q, want the finding with its pointer and kind", out)
	}
	if !strings.Contains(out, "this file is NOT in the change") {
		t.Fatalf("stdout = %q, want the whole point of the judgment said outright", out)
	}
	if !strings.Contains(out, "proposed correction: There are six of them") {
		t.Fatalf("stdout = %q, want the proposed correction printed", out)
	}
	if !strings.Contains(out, "re-read 42 tracked text file(s) under /project in 3 lookup(s)") {
		t.Fatalf("stdout = %q, want what was re-read stated", out)
	}
}

func TestDocumenter_SaysWhenAFindingIsInTheChange(t *testing.T) {
	finding := staleListFinding
	finding.InChange = true
	withDocumentationRun(t, documenter.Result{
		Kind: documenter.Reviewed, Verdict: documenter.DocumentationFalsified,
		Findings: []documenter.Finding{finding}, RequestProvided: true,
	}, nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"document", "--diff", writeDocumenterDiff(t), "--model", "test-model"}, &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stdout.String(), "this file is in the change") {
		t.Fatalf("stdout = %q, want the file's place in the change stated either way", stdout.String())
	}
}

func TestDocumenter_FlagsAClaimItWillNotCorrectOnItsOwn(t *testing.T) {
	// The Client's ruling: never silent, never a guess at intent. The claim
	// and the reason it looks falsified are named where he will see them.
	withDocumentationRun(t, documenter.Result{
		Kind:    documenter.Reviewed,
		Verdict: documenter.DocumentationFalsified,
		Findings: []documenter.Finding{{
			Kind: documenter.FalsifiedClaim, Path: "AGENTS.md", Line: 6,
			Claim:               "A command that needs a fifth one is a design discussion, not a patch.",
			Summary:             "the change added two commands without that discussion",
			NeedsClientJudgment: true,
			JudgmentReason:      "whether the rule still stands is a product call, not a wording fix",
		}},
		RequestProvided: true,
	}, nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"document", "--diff", writeDocumenterDiff(t), "--model", "test-model"}, &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stderr.String(), "claims this change looks to have falsified that inspector will not correct on its own") {
		t.Errorf("stderr = %q, want the flagged claims collected under their own heading", stderr.String())
	}
	if !strings.Contains(stderr.String(), "needs your judgment because: whether the rule still stands is a product call") {
		t.Errorf("stderr = %q, want the reason named", stderr.String())
	}
	if !strings.Contains(stdout.String(), "needs your judgment because: whether the rule still stands is a product call") {
		t.Errorf("stdout = %q, want the reason beside the finding too, never only the summary", stdout.String())
	}
	if strings.Contains(stdout.String(), "proposed correction") {
		t.Fatalf("stdout = %q, must not propose a correction it declined to write", stdout.String())
	}
}

func TestDocumenter_CleanReviewExitsGreen(t *testing.T) {
	withDocumentationRun(t, documenter.Result{
		Kind: documenter.Reviewed, Verdict: documenter.DocumentationConsistent, RequestProvided: true,
	}, nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"document", "--diff", writeDocumenterDiff(t), "--model", "test-model"}, &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stdout.String(), "still describes reality") {
		t.Fatalf("stdout = %q, want a clean result stated as a result", stdout.String())
	}
}

func TestDocumenter_WithoutARequestSaysSoOnEveryRun(t *testing.T) {
	withDocumentationRun(t, documenter.Result{
		Kind: documenter.Reviewed, Verdict: documenter.DocumentationConsistent, RequestProvided: false,
	}, nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"document", "--diff", writeDocumenterDiff(t), "--model", "test-model"}, &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no --request-file was given") {
		t.Fatalf("stdout = %q, want the missing account of intent stated rather than left to read as approval", stdout.String())
	}
}

func TestDocumenter_NamesWhatItCouldNotReRead(t *testing.T) {
	withDocumentationRun(t, documenter.Result{
		Kind: documenter.Reviewed, Verdict: documenter.DocumentationConsistent, RequestProvided: true,
		Corpus: documenter.CorpusSummary{Root: "/project", Files: 3, Skipped: 2, SkippedExamples: []documenter.SkippedFile{
			{Path: "logo.png", Reason: documenter.SkippedBinary},
		}},
	}, nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"document", "--diff", writeDocumenterDiff(t), "--model", "test-model"}, &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stdout.String(), "2 tracked file(s) were left out of the corpus") {
		t.Fatalf("stdout = %q, want a partial corpus named rather than printed as the whole project", stdout.String())
	}
	if !strings.Contains(stdout.String(), "logo.png: not text") {
		t.Fatalf("stdout = %q, want each example skip carrying its reason", stdout.String())
	}
}

func TestDocumenter_ForwardsTheRequestAndRepoToTheReview(t *testing.T) {
	var captured documenter.AgentOptions
	previous := runDocumentation
	runDocumentation = func(_ context.Context, opts documenter.AgentOptions) (documenter.Result, error) {
		captured = opts
		return documenter.Result{Kind: documenter.Reviewed, Verdict: documenter.DocumentationConsistent, RequestProvided: true}, nil
	}
	t.Cleanup(func() { runDocumentation = previous })

	requestPath := filepath.Join(t.TempDir(), "request.txt")
	if err := os.WriteFile(requestPath, []byte("add migrate and rollback commands"), 0o644); err != nil {
		t.Fatalf("writing the request: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"document", "--diff", writeDocumenterDiff(t), "--model", "test-model",
		"--repo", "/some/project", "--request-file", requestPath, "--vendor", "openrouter",
	}, &stdout, &stderr)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if captured.Request != "add migrate and rollback commands" {
		t.Fatalf("AgentOptions.Request = %q, want the request text forwarded", captured.Request)
	}
	if captured.RepoPath != "/some/project" {
		t.Fatalf("AgentOptions.RepoPath = %q, want the repo forwarded", captured.RepoPath)
	}
	if captured.Vendor != "openrouter" {
		t.Fatalf("AgentOptions.Vendor = %q, want the vendor forwarded", captured.Vendor)
	}
	if !strings.Contains(stdout.String(), "vendor: openrouter\n") {
		t.Fatalf("stdout = %q, want the explicit vendor named", stdout.String())
	}
}

func TestDocumenter_DefaultVendorIsNamedExplicitly(t *testing.T) {
	withDocumentationRun(t, documenter.Result{Kind: documenter.Reviewed, Verdict: documenter.DocumentationConsistent}, nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"document", "--diff", writeDocumenterDiff(t), "--model", "test-model"}, &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stdout.String(), "vendor: anthropic (default, no flag given)") {
		t.Fatalf("stdout = %q, want the default vendor choice recorded rather than left silent", stdout.String())
	}
}

func TestDocumenter_RunFailureRefuses(t *testing.T) {
	withDocumentationRun(t, documenter.Result{}, errors.New("model unavailable"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"document", "--diff", writeDocumenterDiff(t), "--model", "test-model"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "model unavailable") {
		t.Fatalf("stderr = %q, want the run failure named", stderr.String())
	}
}

func TestDocumenter_MissingFlagsRefuseWithoutRunning(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no diff", []string{"document", "--model", "test-model"}, "missing required --diff"},
		{"no model", []string{"document", "--diff", "placeholder"}, "missing required --model"},
	}
	for _, test := range cases {
		previous := runDocumentation
		ran := false
		runDocumentation = func(context.Context, documenter.AgentOptions) (documenter.Result, error) {
			ran = true
			return documenter.Result{}, nil
		}
		args := append([]string{}, test.args...)
		if args[len(args)-1] == "placeholder" {
			args[len(args)-1] = writeDocumenterDiff(t)
		}
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		runDocumentation = previous
		if code != exitRefused {
			t.Errorf("%s: exit code = %d, want %d", test.name, code, exitRefused)
		}
		if ran {
			t.Errorf("%s: the review ran anyway", test.name)
		}
		if !strings.Contains(stderr.String(), test.want) {
			t.Errorf("%s: stderr = %q, want %q", test.name, stderr.String(), test.want)
		}
	}
}

func TestDocumenter_UnreadableRequestFileRefuses(t *testing.T) {
	previous := runDocumentation
	ran := false
	runDocumentation = func(context.Context, documenter.AgentOptions) (documenter.Result, error) {
		ran = true
		return documenter.Result{}, nil
	}
	t.Cleanup(func() { runDocumentation = previous })

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"document", "--diff", writeDocumenterDiff(t), "--model", "test-model",
		"--request-file", filepath.Join(t.TempDir(), "missing.txt"),
	}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if ran {
		// Running on without it would judge intent with no account of it.
		t.Fatal("the review ran with a --request-file it could not read")
	}
	if !strings.Contains(stderr.String(), "reading --request-file") {
		t.Fatalf("stderr = %q, want the unreadable request named", stderr.String())
	}
}

func TestDocumenter_UnreadableDiffRefuses(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"document", "--diff", filepath.Join(t.TempDir(), "missing.diff"), "--model", "test-model"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "reading --diff") {
		t.Fatalf("stderr = %q, want a diff-read refusal", stderr.String())
	}
}

func TestDocumenter_RejectsClaimText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"document", "--diff", writeDocumenterDiff(t), "--model", "test-model", "extra", "text"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "does not accept claim text") {
		t.Fatalf("stderr = %q, want a claim-text refusal", stderr.String())
	}
}

func TestDocumenter_HelpIsAUsageErrorNotAVerdict(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"document", "--help"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit code = %d, want %d - a help request is not a verdict attempt", code, exitUsage)
	}
}
