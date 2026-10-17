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
