package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/inactdev/inspector/internal/llm"
	"github.com/inactdev/inspector/internal/reviewer"
)

var runReview = reviewer.RunAgent

func runReviewer(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspector review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	diffPath := fs.String("diff", "", "path to the unified diff of the change to review (required)")
	testPaths := fs.String("test-paths", "", "comma-separated glob patterns naming which files in the change are tests; each changed test is judged for whether it keeps or weakens the exam it imposed")
	vendor := fs.String("vendor", "", "model vendor (default: anthropic); reads <VENDOR>_API_KEY and, for every vendor except anthropic, <VENDOR>_BASE_URL from the environment")
	model := fs.String("model", "", "model name for the selected vendor (required)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: inspector review [flags]

Reads a change and reports anything that looks wrong, each finding pointing
at an exact file and line. Read-only: it never runs the reviewed application
and never edits the reviewed project.

With --test-paths it also judges every changed test file for whether the
change keeps or weakens the exam that test imposed. Without it, the result
says so rather than reading as a change with no test files in it.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	vendorDescription := llm.Describe(*vendor)
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "refused: inspector review does not accept claim text; it derives findings from --diff")
		return exitRefused
	}
	if strings.TrimSpace(*diffPath) == "" {
		fmt.Fprintln(stderr, "refused: missing required --diff")
		return exitRefused
	}
	if strings.TrimSpace(*model) == "" {
		fmt.Fprintln(stderr, "refused: missing required --model")
		return exitRefused
	}
	patterns := splitTestPaths(*testPaths)
	if strings.TrimSpace(*testPaths) != "" && len(patterns) == 0 {
		// Letting this through would silently downgrade the run to one that
		// was never asked to judge tests, which is the one reading of
		// --test-paths nobody intends.
		fmt.Fprintln(stderr, "refused: --test-paths names no usable pattern")
		return exitRefused
	}
	diff, err := os.ReadFile(*diffPath)
	if err != nil {
		result := reviewer.Result{Kind: reviewer.Refused, Message: fmt.Sprintf("reading --diff %s: %v", *diffPath, err)}
		return finishReviewer(vendorDescription, stdout, stderr, result)
	}

	result, err := runReview(context.Background(), reviewer.AgentOptions{
		Diff:             diff,
		TestPathPatterns: patterns,
		Vendor:           *vendor,
		Model:            *model,
	})
	if err != nil {
		result = reviewer.Result{Kind: reviewer.Refused, Message: fmt.Sprintf("review could not be completed: %v", err)}
	}
	return finishReviewer(vendorDescription, stdout, stderr, result)
}

func finishReviewer(vendorDescription string, stdout, stderr io.Writer, result reviewer.Result) int {
	result.Vendor = vendorDescription
	printReviewResult(stdout, stderr, result)
	if result.Kind == reviewer.Refused {
		return exitRefused
	}
	return exitGreen
}

// splitTestPaths reads the comma-separated --test-paths value into patterns.
// An empty element is dropped as the typo it is; a value that yields no
// pattern at all is the caller's error and is refused above.
func splitTestPaths(value string) []string {
	var patterns []string
	for _, pattern := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(pattern); trimmed != "" {
			patterns = append(patterns, trimmed)
		}
	}
	return patterns
}

func printReviewResult(stdout, stderr io.Writer, result reviewer.Result) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err == nil {
		fmt.Fprintln(stdout, string(data))
	}
	fmt.Fprintf(stdout, "vendor: %s\n", result.Vendor)
	switch result.Kind {
	case reviewer.Reviewed:
		printTestChangeVerdict(stdout, result)
		if len(result.Findings) == 0 {
			// "nothing looked wrong" would contradict the verdict printed
			// just above it when that verdict is not a clean one.
			if result.TestChangeVerdict == reviewer.TestChangesWeaken || result.TestChangeVerdict == reviewer.TestChangesUnjudged {
				fmt.Fprintln(stdout, "reviewed - no finding beyond the test changes above")
				return
			}
			fmt.Fprintln(stdout, "reviewed - nothing looked wrong")
			return
		}
		for _, finding := range result.Findings {
			fmt.Fprintf(stdout, "%s:%d: %s\n", finding.Path, finding.Line, finding.Summary)
		}
	case reviewer.Refused:
		fmt.Fprintf(stderr, "refused: %s\n", result.Message)
	}
}

// printTestChangeVerdict always prints the test-change line for a completed
// review, including when no patterns were given: "inspector was not told
// which files are tests" has to be as visible as "no test changed", or the
// absence reads as approval.
func printTestChangeVerdict(stdout io.Writer, result reviewer.Result) {
	fmt.Fprintf(stdout, "test changes: %s\n", result.TestChangeVerdict)
	if result.TestChangeVerdict == reviewer.TestJudgmentNotRequested {
		fmt.Fprintln(stdout, "  no --test-paths pattern was given, so no test change in this diff was judged")
		return
	}
	for _, change := range result.TestChanges {
		judgment := string(change.Judgment)
		if !change.Judged() {
			judgment = "unjudged"
		}
		fmt.Fprintf(stdout, "  %s (%s, +%d/-%d): %s", change.Path, change.Status, change.LinesAdded, change.LinesRemoved, judgment)
		if change.Reason != "" {
			fmt.Fprintf(stdout, " - %s", change.Reason)
		}
		fmt.Fprintln(stdout)
	}
}
