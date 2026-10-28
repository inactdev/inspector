package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/inactdev/inspector/internal/alwaystrue"
	"github.com/inactdev/inspector/internal/inspector"
)

var runAlwaysTrue = alwaystrue.Run

// runAlways is check 3: the project's regression list, driven end to end.
//
// It is its own command and its own status, the same way `inspector examine`
// is, and it is not part of `inspector run`'s publication sequence.
func runAlways(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspector always", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listPath := fs.String("list", alwaystrue.DefaultListName, "path to the project's always-true list")
	commit := fs.String("commit", "", "commit the inspector-always status belongs to (required)")
	owner := fs.String("owner", "", "GitHub repository owner (required)")
	repo := fs.String("repo", "", "GitHub repository name (required)")
	timeout := fs.Duration("timeout", alwaystrue.DefaultTimeout, "maximum duration for the whole run, app start-up included")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `usage: inspector always [flags]

Drives every line of the project's always-true list - the regression list kept
in the repository under its protected paths - and reports whether all of them
still pass.

Each line carries a plain-language promise and the console command, driver
script, or HTTP call that would falsify it. A line with no falsifier is
reported as not runnable and is never reported as passing.

Check 3 brings the app up itself from the image the list names, with its own
fixture data, and removes it afterwards, so a promise is only ever judged
against a known starting state. It mounts no repository at all - that, and not
an instruction it follows, is why it cannot edit what it judges.

One aggregate verdict, never fail-fast: every line is attempted, and the posted
%q status names every broken promise at once. A broken promise posts failure;
an app that would not start or a line that could not be run posts error
instead, and says which.

`, inspector.AlwaysStatusContext)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "refused: inspector always does not accept claim text; it drives the list named by --list")
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

	result, err := runAlwaysTrue(context.Background(), alwaystrue.Options{
		ListPath: *listPath,
		Timeout:  *timeout,
		Stdout:   stdout,
		Stderr:   stderr,
	})
	if err != nil {
		result = alwaystrue.RefusedResult(*listPath, fmt.Sprintf("check 3 could not run: %v", err))
	}
	return finishAlways(*owner, *repo, *commit, stdout, stderr, result)
}

func finishAlways(owner, repo, commit string, stdout, stderr io.Writer, result alwaystrue.Result) int {
	printAlwaysResult(stdout, stderr, result)
	if err := postAlwaysStatus(owner, repo, commit, result); err != nil {
		fmt.Fprintf(stderr, "refused: check 3 reached a %s result but could not post its own status: %v\n", result.Kind, err)
		return exitRefused
	}
	switch {
	case result.Kind == alwaystrue.Red:
		return exitRed
	case result.Kind == alwaystrue.Green && !result.Incomplete:
		return exitGreen
	default:
		return exitRefused
	}
}

// postAlwaysStatus keeps "a promise broke" and "I could not check" as two
// different facts on the pull request page, the same split the examiner makes
// between a found bug and an incomplete examination.
func postAlwaysStatus(owner, repo, commit string, result alwaystrue.Result) error {
	state := inspector.StatusSuccess
	switch {
	case result.Kind == alwaystrue.Red:
		state = inspector.StatusFailure
	case result.Kind == alwaystrue.Refused, result.Incomplete:
		state = inspector.StatusError
	}
	description := statusDescription(inspector.AlwaysStatusContext + ": " + result.StatusDescription())
	return inspector.PostCommitStatus(inspector.PostStatusOptions{
		Owner:       owner,
		Repo:        repo,
		Commit:      commit,
		State:       state,
		Context:     inspector.AlwaysStatusContext,
		Description: description,
		Token:       os.Getenv(inspector.GitHubTokenEnvVar),
		APIBaseURL:  githubAPIBaseURL,
	})
}

func printAlwaysResult(stdout, stderr io.Writer, result alwaystrue.Result) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err == nil {
		fmt.Fprintln(stdout, string(data))
	}
	if result.Kind == alwaystrue.Refused {
		fmt.Fprintf(stderr, "refused: %s\n", result.Message)
		return
	}

	fmt.Fprintf(stdout, "always-true list: %s\n", result.ListPath)
	fmt.Fprintf(stdout, "  %d promise(s) by falsifier shape - %s\n", len(result.Promises), strings.Join(result.ShapeCounts(), ", "))
	if unconfirmed := result.Unconfirmed(); unconfirmed > 0 {
		// Said on every such run: an unaccepted line is still checked and
		// still counted, and leaving that unstated would let a red look like
		// it came from the accepted set.
		fmt.Fprintf(stdout, "  %d of them are proposals no one has confirmed yet; they are driven and counted all the same\n", unconfirmed)
	}
	for _, promise := range result.Promises {
		fmt.Fprintf(stdout, "%s: %s - %s\n", promise.Outcome, promise.ID, promise.Promise)
		fmt.Fprintf(stdout, "  %s\n", promise.Reason)
		if promise.Evidence != "" {
			fmt.Fprintf(stdout, "  observed: %s\n", promise.Evidence)
		}
	}

	switch result.Kind {
	case alwaystrue.Red:
		fmt.Fprintf(stdout, "\nred - %s\n", result.Summary())
		fmt.Fprintf(stderr, "\n%s\nthe project broke a promise it makes:\n", warningBar)
		for _, promise := range result.BrokenPromises() {
			fmt.Fprintf(stderr, "  %s: %s\n    %s\n", promise.ID, promise.Promise, promise.Reason)
		}
		fmt.Fprintf(stderr, "%s\n\n", warningBar)
	default:
		fmt.Fprintf(stdout, "\ngreen - %s\n", result.Summary())
	}
	if result.Incomplete {
		// Distinct from red, and loud: a list that was not fully checked must
		// never read as one where everything held.
		fmt.Fprintf(stderr, "\n%s\nthis run did not check the whole list: %s\n%s\n\n", warningBar, result.Summary(), warningBar)
	}
}
