// Command inspector runs a project's own check command against one exact
// commit and reports green or red. See SPEC.md and README.md.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/inactdev/inspector/internal/inspector"
)

// Exit codes reserve 0, 1, and 2 for verdicts only: 0 green, 1 red, 2
// refused (inspector tried to reach a verdict and could not - see
// inspector.Refused for those causes, plus any infrastructure failure
// Run reports as an error). A local green or red that could not be
// posted as a commit status - see postCommitStatus - also exits 2: an
// unrecorded result proves nothing to inspector-gate, so it is not a
// verdict either. exitUsage is for everything that is not a verdict
// attempt at all - --help, an unrecognized flag, bad usage - so a
// caller can never mistake a help request for a result. 64 follows the
// BSD sysexits.h convention for a command-line usage error (EX_USAGE).
const (
	exitGreen   = 0
	exitRed     = 1
	exitRefused = 2
	exitUsage   = 64
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// githubAPIBaseURL overrides GitHub's REST API host. Empty (the default)
// uses the real API; tests point this at an httptest server instead of
// the network.
var githubAPIBaseURL string

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspector", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoPath := fs.String("repo", ".", "path to the repository to inspect")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `usage: inspector [flags] [claim text...]

Runs the project's own check command against the repo's current HEAD
commit and reports green or red. See README.md for setup.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := inspector.Options{
		RepoPath: *repoPath,
		Claim:    strings.Join(fs.Args(), " "),
		Stdout:   stdout,
		Stderr:   stderr,
	}

	result, err := inspector.Run(opts)
	if err != nil {
		fmt.Fprintf(stderr, "inspector: %v\n", err)
		return exitRefused
	}

	if result.Warning != "" {
		fmt.Fprintf(stderr, "\n%s\n%s\n%s\n\n", warningBar, result.Warning, warningBar)
	}

	switch result.Outcome {
	case inspector.Green:
		fmt.Fprintf(stdout, "\ngreen - %s%s\n", result.Commit, reportSuffix(result.ReportPath))
		if err := postCommitStatus(*repoPath, result, inspector.StatusSuccess); err != nil {
			printStatusFailure(stderr, err)
			return exitRefused
		}
		return exitGreen
	case inspector.Red:
		fmt.Fprintf(stdout, "\nred - %s%s\n", result.Commit, reportSuffix(result.ReportPath))
		if err := postCommitStatus(*repoPath, result, inspector.StatusFailure); err != nil {
			printStatusFailure(stderr, err)
			return exitRefused
		}
		return exitRed
	default: // inspector.Refused
		fmt.Fprintf(stderr, "refused: %s%s\n", result.Message, reportSuffix(result.ReportPath))
		return exitRefused
	}
}

// postCommitStatus records result as a GitHub commit status on the exact
// commit inspector checked - see SPEC.md section 7. A Refused outcome
// never reaches here: its absence already reads as a failure to
// inspector-gate (issue #4), the same as an unreachable API or a missing
// token below, so there is nothing more honest to post for it.
func postCommitStatus(repoPath string, result inspector.Result, state inspector.StatusState) error {
	token := os.Getenv(inspector.GitHubTokenEnvVar)
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("%s is not set - inspector needs a GitHub token with commit-status write access to record its result", inspector.GitHubTokenEnvVar)
	}

	repoRoot, err := inspector.ResolveRepoRoot(repoPath)
	if err != nil {
		return err
	}
	owner, repo, err := inspector.RemoteOwnerRepo(repoRoot)
	if err != nil {
		return err
	}

	description := fmt.Sprintf("inspector: %s", result.Outcome)
	if result.ReportPath != "" {
		description = fmt.Sprintf("inspector: %s - see %s/%s locally", result.Outcome, inspector.RunsDirName, inspector.LatestReportName)
	}

	return inspector.PostCommitStatus(inspector.PostStatusOptions{
		Owner:       owner,
		Repo:        repo,
		Commit:      result.Commit,
		State:       state,
		Description: description,
		Token:       token,
		APIBaseURL:  githubAPIBaseURL,
	})
}

// printStatusFailure makes a failed post impossible to miss: the check
// result printed above it is real, but SPEC.md section 7 treats an
// unrecorded result the same as a failing one, so this must never look
// like the quiet warning a report-save failure gets.
func printStatusFailure(stderr io.Writer, err error) {
	fmt.Fprintf(stderr, "\n%s\ninspector: could not post a commit status: %v\n"+
		"the check result above is real, but without a posted status the gate has nothing to key on -\n"+
		"no result reads the same as a failing one.\n%s\n\n",
		warningBar, err, warningBar)
}

// warningBar makes a Warning impossible to miss among a check command's
// own output, which inspector streams live right above it.
const warningBar = "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"

// reportSuffix formats the report-path annotation, or nothing when
// there is no report - a real possibility now that a save failure
// (Warning) or a refusal that never ran a check both leave it empty.
func reportSuffix(path string) string {
	if path == "" {
		return ""
	}
	return fmt.Sprintf(" (report: %s)", path)
}
