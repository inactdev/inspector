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
// Run reports as an error). A local green that inspector could not
// publish in full - staging it remotely, recording its status, then
// moving the branch - also exits 2: an incomplete publication proves
// nothing to inspector-gate, so it is not a verdict either. exitUsage
// is for everything that is not a verdict
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
var resolvePublicationTarget = inspector.ResolvePublicationTarget

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspector", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoPath := fs.String("repo", ".", "path to the repository to inspect")
	publicationBranch := fs.String("branch", "", "pull request branch to publish after a green result (required)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `usage: inspector [flags] [claim text...]

Runs the project's own check command against the repo's current HEAD
commit and reports green or red. --branch must explicitly name the pull
request branch that inspector may update after a green result. Inspector
puts the commit on GitHub, records its status, then updates only that named
branch, so it needs GITHUB_TOKEN set to a token with commit-status write
access and a GitHub 'origin' remote. See README.md for setup.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*publicationBranch) == "" {
		fmt.Fprintln(stderr, "refused: missing required --branch <pull-request-branch>; inspector never infers a publication branch from the checkout")
		return exitRefused
	}
	if err := inspector.ValidatePublicationBranch(*publicationBranch); err != nil {
		fmt.Fprintf(stderr, "refused: invalid publication branch: %v\n", err)
		return exitRefused
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
		cleanupWarning, err := publishGreen(*repoPath, *publicationBranch, result)
		if err != nil {
			printPublicationFailure(stderr, err)
			return exitRefused
		}
		if cleanupWarning != "" {
			fmt.Fprintf(stderr, "\n%s\n%s\n%s\n\n", warningBar, cleanupWarning, warningBar)
		}
		fmt.Fprintf(stdout, "\ngreen - %s%s\n", result.Commit, reportSuffix(result.ReportPath))
		return exitGreen
	case inspector.Red:
		// v1 deliberately does not publish red work. The local report and
		// this red exit are the record; no remote branch or status is made
		// for a commit inspector did not approve. The Client may overrule
		// this policy in a later version.
		fmt.Fprintf(stdout, "\nred - %s%s (not published by policy)\n", result.Commit, reportSuffix(result.ReportPath))
		return exitRed
	default: // inspector.Refused
		fmt.Fprintf(stderr, "refused: %s%s\n", result.Message, reportSuffix(result.ReportPath))
		return exitRefused
	}
}

// publishGreen performs the publication order that keeps inspector-gate from
// observing a pull-request branch without an inspector status: stage the
// checked commit remotely, record its green status, then move the branch.
// The stage is a non-branch ref and is deleted once the branch moves.
func publishGreen(repoPath, publicationBranch string, result inspector.Result) (cleanupWarning string, err error) {
	if err := inspector.ValidatePublicationBranch(publicationBranch); err != nil {
		return "", err
	}
	token := os.Getenv(inspector.GitHubTokenEnvVar)
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("%s is not set - inspector needs a GitHub token with commit-status write access to publish a green result", inspector.GitHubTokenEnvVar)
	}

	repoRoot, err := inspector.ResolveRepoRoot(repoPath)
	if err != nil {
		return "", err
	}
	target, err := resolvePublicationTarget(repoRoot)
	if err != nil {
		return "", err
	}
	defaultBranch, err := inspector.RepositoryDefaultBranch(inspector.RepositoryOptions{
		Owner:      target.Owner,
		Repo:       target.Repo,
		Token:      token,
		APIBaseURL: githubAPIBaseURL,
	})
	if err != nil {
		return "", err
	}
	if publicationBranch == defaultBranch {
		return "", fmt.Errorf("publication branch %q is the remote default branch; inspector may update only a pull request branch", publicationBranch)
	}
	if err := inspector.ValidatePublicationCommit(repoRoot, result.Commit); err != nil {
		return "", err
	}

	stagingRef := inspector.StagingRefForCommit(result.Commit)
	if err := inspector.PushRefToRemote(repoRoot, target.PushURL, result.Commit, stagingRef); err != nil {
		return "", fmt.Errorf("staging push failed; whether temporary staging ref %s was updated is unknown, and no status post or branch move was attempted: %w", stagingRef, err)
	}

	description := "inspector: green"
	if result.ReportPath != "" {
		description = fmt.Sprintf("inspector: green - see %s/%s locally", inspector.RunsDirName, inspector.LatestReportName)
	}
	if err := inspector.PostCommitStatus(inspector.PostStatusOptions{
		Owner:       target.Owner,
		Repo:        target.Repo,
		Commit:      result.Commit,
		State:       inspector.StatusSuccess,
		Description: description,
		Token:       token,
		APIBaseURL:  githubAPIBaseURL,
	}); err != nil {
		return "", fmt.Errorf("recording the green status failed; whether GitHub accepted it is unknown, temporary staging ref %s was created, and branch %q was not attempted: %w", stagingRef, publicationBranch, err)
	}

	branchRef := "refs/heads/" + publicationBranch
	if err := inspector.PushRefToRemote(repoRoot, target.PushURL, result.Commit, branchRef); err != nil {
		return "", fmt.Errorf("moving branch %q failed after the green status was recorded; whether the remote branch moved is unknown, and temporary staging ref %s may remain: %w", publicationBranch, stagingRef, err)
	}
	if err := inspector.DeleteRefFromRemote(repoRoot, target.PushURL, stagingRef); err != nil {
		return fmt.Sprintf("inspector published green branch %q, but could not confirm removal of its temporary staging ref: %v", publicationBranch, err), nil
	}
	return "", nil
}

// printPublicationFailure makes an incomplete green publication impossible to
// mistake for a verdict. A green check only becomes green to a GitHub reader
// once all three publication steps have completed.
func printPublicationFailure(stderr io.Writer, err error) {
	fmt.Fprintf(stderr, "\n%s\ninspector: could not complete green publication: %v\n"+
		"the local check passed, but the pull-request branch was not confirmed published after the required staging and status steps.\n"+
		"this is an incomplete publication, not a red verdict or an ordinary refusal.\n%s\n\n",
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
