package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/inactdev/inspector/internal/documenter"
	"github.com/inactdev/inspector/internal/llm"
)

var runDocumentation = documenter.RunAgent

func runDocumenter(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspector document", flag.ContinueOnError)
	fs.SetOutput(stderr)
	diffPath := fs.String("diff", "", "path to the unified diff of the change to judge (required)")
	repoPath := fs.String("repo", ".", "path inside the project whose documentation to re-read")
	requestFile := fs.String("request-file", "", "path to the request or expected-outcome text; the only source of what was meant. Omitted, the result says outright that inspector was told nothing, and any finding needing intent is flagged for the Client instead of guessed")
	vendor := fs.String("vendor", "", "model vendor (default: anthropic); reads <VENDOR>_API_KEY and, for every vendor except anthropic, <VENDOR>_BASE_URL from the environment")
	model := fs.String("model", "", "model name for the selected vendor (required)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: inspector document [flags]

Asks two questions about a change, not one: is the change described where it
belongs, and - the one that gets missed - has the change made something
already written untrue?

The second question is answered by re-reading the project's own text, so it
finds the stale document in files the change never touched: a list elsewhere
that no longer lists everything, a described default that changed, an example
command that no longer runs. Inspector builds that corpus itself from the
project's tracked text files; the model can search and read it and nothing
else.

Read-only, and it reports: a correction is proposed text for Fabrica to apply
after the Client's verdict, never an edit inspector makes. It posts no commit
status and gates no merge. A claim that looks falsified but cannot be
corrected without a product decision is flagged for the Client by name
instead of guessed at.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	vendorDescription := llm.Describe(*vendor)
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "refused: inspector document does not accept claim text; it derives findings from --diff and the project's own documentation")
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
		result := documenter.Result{Kind: documenter.Refused, Message: fmt.Sprintf("reading --diff %s: %v", *diffPath, err)}
		return finishDocumenter(vendorDescription, stdout, stderr, result)
	}
	var request string
	if strings.TrimSpace(*requestFile) != "" {
		content, err := os.ReadFile(*requestFile)
		if err != nil {
			// Refused rather than continued without it: a run that silently
			// dropped the request would judge intent with no account of it,
			// which is the one thing SPEC.md section 4 forbids.
			result := documenter.Result{Kind: documenter.Refused, Message: fmt.Sprintf("reading --request-file %s: %v", *requestFile, err)}
			return finishDocumenter(vendorDescription, stdout, stderr, result)
		}
		request = string(content)
	}

	result, err := runDocumentation(context.Background(), documenter.AgentOptions{
		Diff:     diff,
		RepoPath: *repoPath,
		Request:  request,
		Vendor:   *vendor,
		Model:    *model,
	})
	if err != nil {
		result = documenter.Result{Kind: documenter.Refused, Message: fmt.Sprintf("documentation review could not be completed: %v", err)}
	}
	return finishDocumenter(vendorDescription, stdout, stderr, result)
}

func finishDocumenter(vendorDescription string, stdout, stderr io.Writer, result documenter.Result) int {
	result.Vendor = vendorDescription
	printDocumentationResult(stdout, stderr, result)
	if result.Kind == documenter.Refused {
		return exitRefused
	}
	// Like inspector review, this judgment reports and never gates: whatever
	// it found travels with the delivery for the Client to rule on.
	return exitGreen
}

func printDocumentationResult(stdout, stderr io.Writer, result documenter.Result) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err == nil {
		fmt.Fprintln(stdout, string(data))
	}
	fmt.Fprintf(stdout, "vendor: %s\n", result.Vendor)
	if result.Kind == documenter.Refused {
		fmt.Fprintf(stderr, "refused: %s\n", result.Message)
		return
	}

	fmt.Fprintf(stdout, "documentation: %s\n", result.Verdict)
	printCorpusSummary(stdout, result)
	if !result.RequestProvided {
		// Stated on every such run, the same reason the reviewer prints that
		// it was given no test paths: an absence that goes unmentioned reads
		// as approval.
		fmt.Fprintln(stdout, "  no --request-file was given, so inspector had no account of what was meant and guessed at none")
	}
	if len(result.Findings) == 0 {
		fmt.Fprintln(stdout, "reviewed - the project's documentation still describes reality")
		return
	}
	for _, finding := range result.Findings {
		fmt.Fprintf(stdout, "%s:%d: %s - %s\n", finding.Path, finding.Line, finding.Kind, finding.Summary)
		fmt.Fprintf(stdout, "  claim: %s\n", oneLine(finding.Claim))
		if finding.InChange {
			fmt.Fprintln(stdout, "  this file is in the change")
		} else {
			// The whole point of the second question, so it is said outright
			// rather than left for a reader to work out from the diff.
			fmt.Fprintln(stdout, "  this file is NOT in the change - nothing else in the pipeline would have looked here")
		}
		if finding.Correction != "" {
			fmt.Fprintf(stdout, "  proposed correction: %s\n", oneLine(finding.Correction))
		}
		if finding.NeedsClientJudgment {
			fmt.Fprintf(stdout, "  needs your judgment because: %s\n", finding.JudgmentReason)
		}
	}
	printClaimsNeedingTheClient(stderr, result)
}

func printCorpusSummary(stdout io.Writer, result documenter.Result) {
	fmt.Fprintf(stdout, "  re-read %d tracked text file(s) under %s in %d lookup(s)\n", result.Corpus.Files, result.Corpus.Root, result.Lookups)
	if result.Corpus.Skipped == 0 {
		return
	}
	// A judgment made over a partial corpus must never print like one made
	// over the whole project.
	fmt.Fprintf(stdout, "  %d tracked file(s) were left out of the corpus and not re-read, for example:\n", result.Corpus.Skipped)
	for _, skipped := range result.Corpus.SkippedExamples {
		fmt.Fprintf(stdout, "    %s: %s\n", skipped.Path, skipped.Reason)
	}
}

// printClaimsNeedingTheClient collects, on stderr and under its own heading,
// every claim that looks falsified but cannot be corrected without a product
// decision. Per the Client's ruling these are never silent and never guessed:
// they are named together, with their reasons, where a reader scanning a long
// finding list cannot skim past them.
func printClaimsNeedingTheClient(stderr io.Writer, result documenter.Result) {
	flagged := result.NeedsClientJudgment()
	if len(flagged) == 0 {
		return
	}
	fmt.Fprintf(stderr, "\n%s\nclaims this change looks to have falsified that inspector will not correct on its own:\n", warningBar)
	for _, finding := range flagged {
		fmt.Fprintf(stderr, "  %s:%d: %s\n", finding.Path, finding.Line, oneLine(finding.Claim))
		fmt.Fprintf(stderr, "    looks falsified because: %s\n", finding.Summary)
		fmt.Fprintf(stderr, "    needs your judgment because: %s\n", finding.JudgmentReason)
	}
	fmt.Fprintf(stderr, "%s\n\n", warningBar)
}

// oneLine keeps a multi-line claim or correction from breaking the per-finding
// lines apart; the full text is in the JSON above.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
