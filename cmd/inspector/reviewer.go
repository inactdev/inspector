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
	vendor := fs.String("vendor", "", "model vendor (default: anthropic); reads <VENDOR>_API_KEY and, for every vendor except anthropic, <VENDOR>_BASE_URL from the environment")
	model := fs.String("model", "", "model name for the selected vendor (required)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: inspector review [flags]

Reads a change and reports anything that looks wrong, each finding pointing
at an exact file and line. Read-only: it never runs the reviewed application
and never edits the reviewed project.

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
	diff, err := os.ReadFile(*diffPath)
	if err != nil {
		result := reviewer.Result{Kind: reviewer.Refused, Message: fmt.Sprintf("reading --diff %s: %v", *diffPath, err)}
		return finishReviewer(vendorDescription, stdout, stderr, result)
	}

	result, err := runReview(context.Background(), reviewer.AgentOptions{
		Diff:   diff,
		Vendor: *vendor,
		Model:  *model,
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

func printReviewResult(stdout, stderr io.Writer, result reviewer.Result) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err == nil {
		fmt.Fprintln(stdout, string(data))
	}
	fmt.Fprintf(stdout, "vendor: %s\n", result.Vendor)
	switch result.Kind {
	case reviewer.Reviewed:
		if len(result.Findings) == 0 {
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
