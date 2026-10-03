package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/inactdev/inspector/internal/examiner"
	"github.com/inactdev/inspector/internal/inspector"
)

var runExamination = examiner.Run

func runExaminer(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspector examine", flag.ContinueOnError)
	fs.SetOutput(stderr)
	requestPath := fs.String("request", "", "path to the issue or pull request request text (required)")
	guidebookPath := fs.String("guidebook", "", "path to the app guidebook (required)")
	testChangesPath := fs.String("test-changes", "", "path to Fabrica's base-diffed test-change list (required)")
	appURL := fs.String("app-url", "", "URL of the already-running app (required)")
	commit := fs.String("commit", "", "commit the examiner status belongs to (required)")
	owner := fs.String("owner", "", "GitHub repository owner (required)")
	repo := fs.String("repo", "", "GitHub repository name (required)")
	model := fs.String("model", "", "Anthropic model for deriving and driving scenarios (required)")
	image := fs.String("image", examiner.DefaultImage, "generic runtime image for the sealed examiner")
	agentBinary := fs.String("agent-binary", "", "Linux Inspector binary used inside the sealed container")
	network := fs.String("network", examiner.DefaultNetwork, "Docker network that reaches the running app")
	timeout := fs.Duration("timeout", examiner.DefaultTimeout, "maximum examination duration")
	apiBaseURL := fs.String("api-base-url", "", "Anthropic Messages API URL (testing only)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: inspector examine [flags]

Judges request-derived outcomes by driving an already-running HTTP app. The
examiner receives only the request, guidebook, and base-diffed test changes in
a sealed container; it never mounts the judged repository or implementation.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "refused: inspector examine does not accept claim text; it derives outcomes from --request")
		return exitRefused
	}
	for _, required := range []struct{ name, value string }{
		{"--request", *requestPath}, {"--guidebook", *guidebookPath}, {"--test-changes", *testChangesPath},
		{"--app-url", *appURL}, {"--commit", *commit}, {"--owner", *owner}, {"--repo", *repo}, {"--model", *model},
	} {
		if strings.TrimSpace(required.value) == "" {
			fmt.Fprintf(stderr, "refused: missing required %s\n", required.name)
			return exitRefused
		}
	}

	result, err := runExamination(context.Background(), examiner.RunOptions{
		Inputs: examiner.Inputs{
			RequestPath:     *requestPath,
			GuidebookPath:   *guidebookPath,
			TestChangesPath: *testChangesPath,
		},
		AppURL:     *appURL,
		Model:      *model,
		APIBaseURL: *apiBaseURL,
		Image:      *image,
		Network:    *network,
		Executable: *agentBinary,
		Timeout:    *timeout,
		Stdout:     stdout,
		Stderr:     stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "refused: examiner could not start: %v\n", err)
		return exitRefused
	}
	if err := postExaminerStatus(*owner, *repo, *commit, result); err != nil {
		fmt.Fprintf(stderr, "refused: examiner reached a %s result but could not post its separate status: %v\n", result.Kind, err)
		return exitRefused
	}
	printExaminerResult(stdout, stderr, result)
	switch result.Kind {
	case examiner.Green:
		return exitGreen
	case examiner.Red:
		return exitRed
	default:
		return exitRefused
	}
}

func postExaminerStatus(owner, repo, commit string, result examiner.Result) error {
	state := inspector.StatusSuccess
	description := "examiner: every claimed capability confirmed"
	switch result.Kind {
	case examiner.Red:
		state = inspector.StatusFailure
		description = examinerFailureDescription(result)
	case examiner.Refused:
		state = inspector.StatusError
		description = "examiner: could not be tested"
	}
	return inspector.PostCommitStatus(inspector.PostStatusOptions{
		Owner:       owner,
		Repo:        repo,
		Commit:      commit,
		State:       state,
		Context:     inspector.ExaminerStatusContext,
		Description: description,
		Token:       os.Getenv(inspector.GitHubTokenEnvVar),
		APIBaseURL:  githubAPIBaseURL,
	})
}

func examinerFailureDescription(result examiner.Result) string {
	for _, outcome := range result.Verdict.Outcomes {
		if outcome.Verdict == examiner.NotConfirmed {
			return statusDescription("examiner: missing " + outcome.Claim)
		}
	}
	for _, finding := range result.Verdict.Findings {
		return statusDescription("examiner: " + finding.TestPath + " stopped checking " + finding.Detail)
	}
	return "examiner: requested behavior was not confirmed"
}

func statusDescription(description string) string {
	const max = 140
	if len(description) <= max {
		return description
	}
	return description[:max-3] + "..."
}

func printExaminerResult(stdout, stderr io.Writer, result examiner.Result) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err == nil {
		fmt.Fprintln(stdout, string(data))
	}
	for _, outcome := range result.Verdict.Outcomes {
		switch outcome.Verdict {
		case examiner.Confirmed:
			fmt.Fprintf(stdout, "confirmed: %s\n", outcome.Claim)
		case examiner.NotConfirmed:
			fmt.Fprintf(stdout, "not confirmed: %s\nproposed regression: %s\n", outcome.Claim, outcome.ProposedRegression)
		case examiner.CouldNotBeTested:
			fmt.Fprintf(stderr, "could not be tested: %s - %s\n", outcome.Claim, outcome.Evidence)
		}
	}
	for _, finding := range result.Verdict.Findings {
		fmt.Fprintf(stdout, "not confirmed: test %s stopped checking %s\nproposed regression: %s\n", finding.TestPath, finding.Detail, finding.ProposedRegression)
	}
	switch result.Kind {
	case examiner.Green:
		fmt.Fprintln(stdout, "green - every claimed capability was confirmed")
	case examiner.Red:
		fmt.Fprintln(stdout, "red - one or more claimed capabilities or test protections were not confirmed")
	case examiner.Refused:
		fmt.Fprintf(stderr, "refused: %s\n", result.Message)
	}
}

func runExaminerAgent(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspector examiner-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	inputDir := fs.String("input-dir", "", "sealed input directory")
	outputDir := fs.String("output-dir", "", "sealed output directory")
	appURL := fs.String("app-url", "", "running app URL")
	model := fs.String("model", "", "Anthropic model")
	apiBaseURL := fs.String("api-base-url", "", "Anthropic Messages API URL")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	for _, required := range []struct{ name, value string }{
		{"--input-dir", *inputDir}, {"--output-dir", *outputDir}, {"--app-url", *appURL}, {"--model", *model},
	} {
		if strings.TrimSpace(required.value) == "" {
			fmt.Fprintf(stderr, "examiner-agent: missing required %s\n", required.name)
			return exitUsage
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), examiner.DefaultTimeout)
	defer cancel()
	if err := examiner.RunAgent(ctx, examiner.AgentOptions{
		InputDir: *inputDir, OutputDir: *outputDir, AppURL: *appURL, Model: *model, APIBaseURL: *apiBaseURL,
	}); err != nil {
		fmt.Fprintf(stderr, "examiner-agent: %v\n", err)
		return exitRefused
	}
	return exitGreen
}
