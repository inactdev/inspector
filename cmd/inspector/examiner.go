package main

import (
	"context"
	"encoding/hex"
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
	requestPath := fs.String("request", "", "path to the issue or pull request text (required)")
	featureMapPath := fs.String("feature-map", "", "path to the project feature map (required)")
	alwaysTruePath := fs.String("always-true", "", "path to the project always-true list (required)")
	changedFilesPath := fs.String("changed-files", "", "path to Fabrica's names-only changed-file list (required)")
	baseTestsPath := fs.String("base-tests", "", "path to pre-task changed test versions (required)")
	appURL := fs.String("app-url", "", "URL of the already-running app (required)")
	commit := fs.String("commit", "", "commit the examiner status belongs to (required)")
	owner := fs.String("owner", "", "GitHub repository owner (required)")
	repo := fs.String("repo", "", "GitHub repository name (required)")
	model := fs.String("model", "", "Anthropic model for deriving and driving scenarios (required)")
	network := fs.String("network", examiner.DefaultNetwork, "Docker network that reaches the running app")
	timeout := fs.Duration("timeout", examiner.DefaultTimeout, "maximum examination duration")
	budget := fs.Int("budget", examiner.DefaultBudget, "maximum app-driving attempts")
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: inspector examine [flags]

Judges request-derived outcomes by driving an already-running HTTP app. The
examiner receives only request text, a feature map, an always-true list, names
of changed files, and pre-task changed tests in a sealed container; it never
mounts the judged repository or implementation.

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
		{"--owner", *owner}, {"--repo", *repo}, {"--commit", *commit},
	} {
		if strings.TrimSpace(required.value) == "" {
			fmt.Fprintf(stderr, "refused: missing required %s\n", required.name)
			return exitRefused
		}
	}
	if !isFullCommitSHA(*commit) {
		fmt.Fprintln(stderr, "refused: --commit must be a full 40-character commit SHA")
		return exitRefused
	}
	for _, required := range []struct{ name, value string }{
		{"--request", *requestPath}, {"--feature-map", *featureMapPath}, {"--always-true", *alwaysTruePath},
		{"--changed-files", *changedFilesPath}, {"--base-tests", *baseTestsPath}, {"--app-url", *appURL}, {"--model", *model},
	} {
		if strings.TrimSpace(required.value) == "" {
			result := examiner.Result{Kind: examiner.Refused, Message: fmt.Sprintf("missing required %s", required.name)}
			return finishExaminer(*owner, *repo, *commit, stdout, stderr, result)
		}
	}

	result, err := runExamination(context.Background(), examiner.RunOptions{
		Inputs: examiner.Inputs{
			RequestPath:      *requestPath,
			FeatureMapPath:   *featureMapPath,
			AlwaysTruePath:   *alwaysTruePath,
			ChangedFilesPath: *changedFilesPath,
			BaseTestsPath:    *baseTestsPath,
		},
		AppURL:  *appURL,
		Model:   *model,
		Network: *network,
		Timeout: *timeout,
		Budget:  *budget,
		Stdout:  stdout,
		Stderr:  stderr,
	})
	if err != nil {
		result = examiner.Result{Kind: examiner.Refused, Message: fmt.Sprintf("examiner could not start: %v", err)}
	}
	return finishExaminer(*owner, *repo, *commit, stdout, stderr, result)
}

func finishExaminer(owner, repo, commit string, stdout, stderr io.Writer, result examiner.Result) int {
	printExaminerResult(stdout, stderr, result)
	if err := postExaminerStatus(owner, repo, commit, result); err != nil {
		fmt.Fprintf(stderr, "refused: examiner reached a %s result but could not post its separate status: %v\n", result.Kind, err)
		return exitRefused
	}
	switch result.Kind {
	case examiner.Green:
		return exitGreen
	case examiner.Red:
		return exitRed
	default:
		return exitRefused
	}
}

func isFullCommitSHA(commit string) bool {
	if len(commit) != 40 {
		return false
	}
	_, err := hex.DecodeString(commit)
	return err == nil
}

func postExaminerStatus(owner, repo, commit string, result examiner.Result) error {
	state := inspector.StatusSuccess
	description := "examiner: every claimed capability confirmed"
	switch {
	case result.Kind == examiner.Red:
		state = inspector.StatusFailure
		description = examinerFailureDescription(result)
	case result.Verdict.ExaminationIncomplete:
		state = inspector.StatusError
		description = examinerIncompleteDescription(result)
	case result.Kind == examiner.Refused:
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
	parts := []string{"examiner:"}
	for _, outcome := range result.Verdict.Outcomes {
		if outcome.Verdict == examiner.NotConfirmed {
			parts = append(parts, "missing "+outcome.Claim)
			break
		}
	}
	if len(parts) == 1 {
		for _, finding := range result.Verdict.Findings {
			parts = append(parts, finding.TestPath+" stopped checking "+finding.Detail)
			break
		}
	}
	if result.Verdict.ExaminationIncomplete {
		parts = append(parts, "examination incomplete: "+untestedClaims(result))
	}
	if len(parts) == 1 {
		parts = append(parts, "requested behavior was not confirmed")
	}
	return statusDescription(strings.Join(parts, "; "))
}

func examinerIncompleteDescription(result examiner.Result) string {
	return statusDescription("examiner: examination incomplete: " + untestedClaims(result))
}

func untestedClaims(result examiner.Result) string {
	for _, outcome := range result.Verdict.Outcomes {
		if outcome.Verdict == examiner.CouldNotBeTested {
			return "could not test " + outcome.Claim
		}
	}
	return "one or more capabilities could not be tested"
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
	if result.Verdict.ExaminationIncomplete {
		fmt.Fprintf(stderr, "examination incomplete: %s\n", untestedClaims(result))
	}
}
