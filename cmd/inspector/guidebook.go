package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/inactdev/inspector/internal/guidebook"
	"github.com/inactdev/inspector/internal/llm"
)

var (
	generateGuidebook = guidebook.RunAgent
	prepareGuidebook  = guidebook.Prepare
)

func runGuidebook(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspector guidebook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoPath := fs.String("repo", ".", "path inside the project to write a guidebook for")
	vendor := fs.String("vendor", "", "model vendor (default: anthropic); reads <VENDOR>_API_KEY and, for every vendor except anthropic, <VENDOR>_BASE_URL from the environment")
	model := fs.String("model", "", "model name for the selected vendor (required unless --facts-only)")
	factsOnly := fs.Bool("facts-only", false, "print the facts inspector extracts and stop: no model is called, no credential is needed, and nothing is written")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `usage: inspector guidebook [flags]

Writes the examiner's guidebook for a project: what the app's features are, how
to start it, how to drive each feature, how to observe the result, and what
cannot be tested here.

The examiner (inspector examine) is given a running app and this document, and
never the source - so this command has exactly the access the examiner is
denied, and converts it into operating instructions once. It is a setup-time
act. Nothing regenerates a guidebook during an examination.

Inspector extracts the project's deterministic facts itself - the check
command, manifests, run scripts, compose services, published ports, routes, CLI
entry points, environment variables - each with the file and line it came from,
and lists them verbatim at the end of the document. The model writes the prose
over those facts and must cite them, so a reader can tell what was read from
what was concluded. --facts-only prints just that half.

The guidebook is written to %s at the project root and is a DRAFT: it carries
%s: no until a person follows it and replaces that line
with a date and their name. An examination driven from an unconfirmed guidebook
records every capability as could-not-be-tested rather than as a pass.

Running it twice never costs you a confirmed guidebook. When %s
already exists it is left untouched and the new document is written to
%s beside it.

flags:
`, guidebook.FileName, "`confirmed`", guidebook.FileName, guidebook.DraftFileName)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	vendorDescription := llm.Describe(*vendor)
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "refused: inspector guidebook does not accept free text; it derives the guidebook from the project at --repo")
		return exitRefused
	}
	if *factsOnly {
		return printExtractedFacts(*repoPath, stdout, stderr)
	}
	if strings.TrimSpace(*model) == "" {
		fmt.Fprintln(stderr, "refused: missing required --model")
		return exitRefused
	}

	result, document, err := generateGuidebook(context.Background(), guidebook.AgentOptions{
		RepoPath: *repoPath,
		Vendor:   *vendor,
		Model:    *model,
	})
	if err != nil {
		result = guidebook.Result{Kind: guidebook.Refused, Message: fmt.Sprintf("no guidebook was written: %v", err)}
		return finishGuidebook(vendorDescription, stdout, stderr, result)
	}
	// Written only once the whole document validated and rendered, so a
	// rejected or half-finished generation leaves no file for somebody to
	// mistake for a guidebook.
	outcome, err := guidebook.Write(result.Corpus.Root, document)
	if err != nil {
		result = guidebook.Result{Kind: guidebook.Refused, Message: fmt.Sprintf("the guidebook was generated but could not be written: %v", err)}
		return finishGuidebook(vendorDescription, stdout, stderr, result)
	}
	result.Path = outcome.Path
	result.Draft = outcome.Draft
	result.ReplacedDraft = outcome.ReplacedDraft
	result.Message = outcome.Describe()
	return finishGuidebook(vendorDescription, stdout, stderr, result)
}

// printExtractedFacts is the deterministic half on its own: what inspector read
// out of the project, with no model and no credential. It writes nothing.
func printExtractedFacts(repoPath string, stdout, stderr io.Writer) int {
	_, facts, err := prepareGuidebook(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "refused: %v\n", err)
		return exitRefused
	}
	fmt.Fprintf(stdout, "project: %s\n", facts.Project)
	if facts.Commit != "" {
		fmt.Fprintf(stdout, "commit: %s\n", facts.Commit)
	}
	fmt.Fprintf(stdout, "%d fact(s) from %d scanned file(s)\n\n", len(facts.Facts), facts.Scanned)
	fmt.Fprint(stdout, facts.Render())
	fmt.Fprintln(stdout, "\nnothing was written; these are the facts a generated guidebook would be grounded on")
	return exitGreen
}

func finishGuidebook(vendorDescription string, stdout, stderr io.Writer, result guidebook.Result) int {
	result.Vendor = vendorDescription
	printGuidebookResult(stdout, stderr, result)
	if result.Kind == guidebook.Refused {
		return exitRefused
	}
	// Like inspector review and inspector document, this posts no status and
	// gates no merge: a guidebook is setup-time work a person then confirms.
	return exitGreen
}

func printGuidebookResult(stdout, stderr io.Writer, result guidebook.Result) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err == nil {
		fmt.Fprintln(stdout, string(data))
	}
	fmt.Fprintf(stdout, "vendor: %s\n", result.Vendor)
	if result.Kind == guidebook.Refused {
		fmt.Fprintf(stderr, "refused: %s\n", result.Message)
		return
	}
	fmt.Fprintf(stdout, "project: %s\n", result.Project)
	fmt.Fprintf(stdout, "read %d tracked text file(s) under %s in %d lookup(s), and extracted %d fact(s)\n",
		result.Corpus.Files, result.Corpus.Root, result.Lookups, result.Facts)
	if result.Corpus.Skipped > 0 {
		// A guidebook written over a partial corpus must never read like one
		// written over the whole project.
		fmt.Fprintf(stdout, "  %d tracked file(s) were left out of the corpus and never read, for example:\n", result.Corpus.Skipped)
		for _, skipped := range result.Corpus.SkippedExamples {
			fmt.Fprintf(stdout, "    %s: %s\n", skipped.Path, skipped.Reason)
		}
	}
	for _, capped := range result.FactsCap {
		fmt.Fprintf(stdout, "  capped: %s\n", capped)
	}
	fmt.Fprintf(stdout, "features: %d\n", len(result.Features))
	for _, feature := range result.Features {
		fmt.Fprintf(stdout, "  %s\n", feature)
	}
	fmt.Fprintf(stdout, "%s\n", result.Message)
	// On stderr and under the bar, because a guidebook taken as authority is
	// the one failure this whole design is built to prevent: the examiner would
	// be driving a project from instructions nobody checked.
	fmt.Fprintf(stderr, "\n%s\nthis guidebook is UNCONFIRMED. Follow every command in %s yourself, correct what is wrong,\nthen replace its %q line with today's date and your name.\nuntil then, an examination driven from it records every capability as could-not-be-tested, never as a pass.\n%s\n\n",
		warningBar, result.Path, "confirmed", warningBar)
}
