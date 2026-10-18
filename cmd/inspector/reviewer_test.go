package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/reviewer"
)

const reviewerTestDiff = `diff --git a/greet.go b/greet.go
index abc1234..def5678 100644
--- a/greet.go
+++ b/greet.go
@@ -1,2 +1,2 @@
 package greet
-func Hello() string { return "hi" }
+func Hello() string { return "h" + "i" }
`

func writeReviewerDiff(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "change.diff")
	if err := os.WriteFile(path, []byte(reviewerTestDiff), 0o644); err != nil {
		t.Fatalf("writing diff fixture: %v", err)
	}
	return path
}

func withReviewRun(t *testing.T, result reviewer.Result, runErr error) {
	t.Helper()
	previous := runReview
	runReview = func(context.Context, reviewer.AgentOptions) (reviewer.Result, error) { return result, runErr }
	t.Cleanup(func() { runReview = previous })
}

func TestReviewer_ReportsFindingsAndExitsGreen(t *testing.T) {
	withReviewRun(t, reviewer.Result{Kind: reviewer.Reviewed, Findings: []reviewer.Finding{
		{Path: "greet.go", Line: 2, Summary: "string concatenation is needlessly indirect"},
	}}, nil)

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t), "--model", "test-model"}, &stdout, &stderr)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stdout.String(), "greet.go:2: string concatenation is needlessly indirect") {
		t.Fatalf("stdout = %q, want the finding printed with its pointer", stdout.String())
	}
	if !strings.Contains(stdout.String(), "vendor: anthropic (default, no flag given)") {
		t.Fatalf("stdout = %q, want the default vendor named explicitly", stdout.String())
	}
}

func TestReviewer_CleanReviewExitsGreenWithoutFindings(t *testing.T) {
	withReviewRun(t, reviewer.Result{Kind: reviewer.Reviewed}, nil)

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t), "--model", "test-model"}, &stdout, &stderr)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stdout.String(), "reviewed - nothing looked wrong") {
		t.Fatalf("stdout = %q, want a clean-review message", stdout.String())
	}
}

func TestReviewer_RunFailureRefuses(t *testing.T) {
	withReviewRun(t, reviewer.Result{}, errors.New("model unavailable"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t), "--model", "test-model"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "model unavailable") {
		t.Fatalf("stderr = %q, want the run failure named", stderr.String())
	}
}

func TestReviewer_MissingDiffFlagRefusesWithoutRunning(t *testing.T) {
	previous := runReview
	runCalled := false
	runReview = func(context.Context, reviewer.AgentOptions) (reviewer.Result, error) {
		runCalled = true
		return reviewer.Result{}, nil
	}
	t.Cleanup(func() { runReview = previous })

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--model", "test-model"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if runCalled {
		t.Fatal("review ran without a --diff")
	}
	if !strings.Contains(stderr.String(), "missing required --diff") {
		t.Fatalf("stderr = %q, want missing --diff refusal", stderr.String())
	}
}

func TestReviewer_MissingModelFlagRefusesWithoutRunning(t *testing.T) {
	previous := runReview
	runCalled := false
	runReview = func(context.Context, reviewer.AgentOptions) (reviewer.Result, error) {
		runCalled = true
		return reviewer.Result{}, nil
	}
	t.Cleanup(func() { runReview = previous })

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t)}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if runCalled {
		t.Fatal("review ran without a --model")
	}
	if !strings.Contains(stderr.String(), "missing required --model") {
		t.Fatalf("stderr = %q, want missing --model refusal", stderr.String())
	}
}

func TestReviewer_VendorFlagOverridesTheDefaultEverywhere(t *testing.T) {
	var capturedVendor string
	previous := runReview
	runReview = func(_ context.Context, opts reviewer.AgentOptions) (reviewer.Result, error) {
		capturedVendor = opts.Vendor
		return reviewer.Result{Kind: reviewer.Reviewed}, nil
	}
	t.Cleanup(func() { runReview = previous })

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t), "--model", "test-model", "--vendor", "openrouter"}, &stdout, &stderr)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if capturedVendor != "openrouter" {
		t.Fatalf("AgentOptions.Vendor = %q, want openrouter forwarded from the flag", capturedVendor)
	}
	if !strings.Contains(stdout.String(), "vendor: openrouter\n") {
		t.Fatalf("stdout = %q, want the explicit vendor named without a default marker", stdout.String())
	}
}

func TestReviewer_TestPathsFlowThroughToTheReview(t *testing.T) {
	var capturedPatterns []string
	previous := runReview
	runReview = func(_ context.Context, opts reviewer.AgentOptions) (reviewer.Result, error) {
		capturedPatterns = opts.TestPathPatterns
		return reviewer.Result{Kind: reviewer.Reviewed, TestChangeVerdict: reviewer.NoTestChanges}, nil
	}
	t.Cleanup(func() { runReview = previous })

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t), "--model", "test-model", "--test-paths", "*_test.go, internal/*_test.go"}, &stdout, &stderr)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if len(capturedPatterns) != 2 || capturedPatterns[0] != "*_test.go" || capturedPatterns[1] != "internal/*_test.go" {
		t.Fatalf("AgentOptions.TestPathPatterns = %#v, want both patterns trimmed and forwarded", capturedPatterns)
	}
	if !strings.Contains(stdout.String(), "test changes: no_test_changes") {
		t.Fatalf("stdout = %q, want the derived test-change verdict printed", stdout.String())
	}
}

func TestReviewer_PrintsAWeakenedTestChangeButStillExitsGreen(t *testing.T) {
	// SPEC.md's reviewer reports and never gates: a weakened exam travels
	// with the delivery for the Client to rule on, it is not a merge block.
	withReviewRun(t, reviewer.Result{
		Kind:              reviewer.Reviewed,
		TestChangeVerdict: reviewer.TestChangesWeaken,
		TestChanges: []reviewer.TestChange{{
			Path: "greet_test.go", Status: reviewer.TestFileModified, LinesAdded: 1, LinesRemoved: 3,
			Judgment: reviewer.ExamWeakened, Reason: "the only assertion on Hello's output was dropped",
		}},
	}, nil)

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t), "--model", "test-model", "--test-paths", "*_test.go"}, &stdout, &stderr)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d - the reviewer reports, it never gates", code, exitGreen)
	}
	if !strings.Contains(stdout.String(), "test changes: test_changes_weaken_the_exam") {
		t.Fatalf("stdout = %q, want the weakened verdict named", stdout.String())
	}
	if !strings.Contains(stdout.String(), "greet_test.go (modified, +1/-3): weakens_the_exam - the only assertion on Hello's output was dropped") {
		t.Fatalf("stdout = %q, want the path, its diff facts, the judgment and the reason", stdout.String())
	}
	if strings.Contains(stdout.String(), "nothing looked wrong") {
		t.Fatalf("stdout = %q, must not say nothing looked wrong under a weakened verdict", stdout.String())
	}
}

func TestReviewer_PrintsAnUnjudgedTestChangeAsUnjudged(t *testing.T) {
	withReviewRun(t, reviewer.Result{
		Kind:              reviewer.Reviewed,
		TestChangeVerdict: reviewer.TestChangesUnjudged,
		TestChanges:       []reviewer.TestChange{{Path: "greet_test.go", Status: reviewer.TestFileDeleted, LinesRemoved: 9}},
	}, nil)

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t), "--model", "test-model", "--test-paths", "*_test.go"}, &stdout, &stderr)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stdout.String(), "greet_test.go (deleted, +0/-9): unjudged") {
		t.Fatalf("stdout = %q, want the unjudged change named rather than left blank", stdout.String())
	}
}

func TestReviewer_WithoutTestPathsSaysSoOnEveryRun(t *testing.T) {
	withReviewRun(t, reviewer.Result{Kind: reviewer.Reviewed, TestChangeVerdict: reviewer.TestJudgmentNotRequested}, nil)

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t), "--model", "test-model"}, &stdout, &stderr)
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no --test-paths pattern was given, so no test change in this diff was judged") {
		t.Fatalf("stdout = %q, want the omission stated outright", stdout.String())
	}
}

func TestReviewer_TestPathsWithNoUsablePatternRefusesWithoutRunning(t *testing.T) {
	previous := runReview
	runCalled := false
	runReview = func(context.Context, reviewer.AgentOptions) (reviewer.Result, error) {
		runCalled = true
		return reviewer.Result{}, nil
	}
	t.Cleanup(func() { runReview = previous })

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t), "--model", "test-model", "--test-paths", " , "}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if runCalled {
		t.Fatal("review ran with a --test-paths value that names no pattern, which would silently judge nothing")
	}
	if !strings.Contains(stderr.String(), "names no usable pattern") {
		t.Fatalf("stderr = %q, want the unusable --test-paths value refused by name", stderr.String())
	}
}

func TestReviewer_RejectsClaimText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", writeReviewerDiff(t), "--model", "test-model", "extra", "text"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "does not accept claim text") {
		t.Fatalf("stderr = %q, want a claim-text refusal", stderr.String())
	}
}

func TestReviewer_UnreadableDiffRefuses(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--diff", filepath.Join(t.TempDir(), "missing.diff"), "--model", "test-model"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "reading --diff") {
		t.Fatalf("stderr = %q, want a diff-read refusal", stderr.String())
	}
}
