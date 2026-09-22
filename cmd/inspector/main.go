// Command inspector runs a project's own check command against one exact
// commit and reports green or red. See SPEC.md and README.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/inactdev/inspector/internal/inspector"
)

// Exit codes reserve 0, 1, and 2 for verdict attempts: 0 green, 1 red,
// and 2 no verdict. A refusal uses 2 for any failure before remote
// publication starts and never attempts to push or publish. Once the
// staging push begins, a local green that inspector cannot publish in
// full - recording its status, then moving the branch - also uses 2,
// but is reported as an incomplete publication rather than a refusal.
// exitUsage is for
// everything that is not a verdict attempt at all - --help, an
// unrecognized flag, bad usage - so a
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
var validatePublicationPlatform = inspector.ValidatePublicationPlatform

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
	publicationTarget := capturePublicationTarget(*repoPath)
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
	if err := validatePostCheck(publicationTarget, result); err != nil {
		fmt.Fprintf(stderr, "refused: %v%s\n", err, reportSuffix(result.ReportPath))
		return exitRefused
	}

	if result.Warning != "" {
		fmt.Fprintf(stderr, "\n%s\n%s\n%s\n\n", warningBar, result.Warning, warningBar)
	}

	switch result.Outcome {
	case inspector.Green:
		cleanupWarning, err := publishGreen(publicationTarget, *publicationBranch, result)
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

type publicationTargetSnapshot struct {
	repoRoot        string
	target          inspector.PublicationTarget
	validation      inspector.ValidationPolicy
	targetErr       error
	validationError error
}

func capturePublicationTarget(repoPath string) publicationTargetSnapshot {
	repoRoot, err := inspector.ResolveRepoRoot(repoPath)
	if err != nil {
		return publicationTargetSnapshot{targetErr: err}
	}
	target, targetErr := resolvePublicationTarget(repoRoot)
	validation, validationError := inspector.CaptureValidationPolicy(repoRoot)
	return publicationTargetSnapshot{
		repoRoot:        repoRoot,
		target:          target,
		validation:      validation,
		targetErr:       targetErr,
		validationError: validationError,
	}
}

func validatePostCheck(snapshot publicationTargetSnapshot, result inspector.Result) error {
	if result.Commit == "" {
		return nil
	}
	if snapshot.validationError != nil {
		return fmt.Errorf("capturing pre-check validation state: %w", snapshot.validationError)
	}
	return inspector.ValidatePublicationCommit(snapshot.repoRoot, result.Commit, snapshot.validation)
}

type publicationError struct {
	cause     error
	attempted bool
}

func (e *publicationError) Error() string {
	return e.cause.Error()
}

func (e *publicationError) Unwrap() error {
	return e.cause
}

func publicationRefusal(err error) error {
	return &publicationError{cause: err}
}

func incompletePublication(err error) error {
	return &publicationError{cause: err, attempted: true}
}

// publishGreen performs the publication order that keeps inspector-gate from
// observing a pull-request branch without an inspector status: stage the
// checked commit remotely, record its green status, then move the branch.
// The stage is a non-branch ref; its deletion is attempted once the branch moves.
func publishGreen(snapshot publicationTargetSnapshot, publicationBranch string, result inspector.Result) (cleanupWarning string, err error) {
	if err := validatePublicationPlatform(); err != nil {
		return "", publicationRefusal(err)
	}
	if err := inspector.ValidatePublicationBranch(publicationBranch); err != nil {
		return "", publicationRefusal(err)
	}
	token := os.Getenv(inspector.GitHubTokenEnvVar)
	if strings.TrimSpace(token) == "" {
		return "", publicationRefusal(fmt.Errorf("%s is not set - inspector needs a GitHub token with commit-status write access to publish a green result", inspector.GitHubTokenEnvVar))
	}

	if snapshot.targetErr != nil {
		return "", publicationRefusal(snapshot.targetErr)
	}
	if snapshot.validationError != nil {
		return "", publicationRefusal(snapshot.validationError)
	}
	if err := inspector.ValidatePublicationCommit(snapshot.repoRoot, result.Commit, snapshot.validation); err != nil {
		return "", publicationRefusal(err)
	}
	currentTarget, err := resolvePublicationTarget(snapshot.repoRoot)
	if err != nil {
		return "", publicationRefusal(fmt.Errorf("publication target changed during inspection: %w", err))
	}
	if currentTarget != snapshot.target {
		return "", publicationRefusal(fmt.Errorf("publication target changed during inspection; refusing to publish to either destination"))
	}
	defaultBranch, err := inspector.RepositoryDefaultBranch(inspector.RepositoryOptions{
		Owner:      snapshot.target.Owner,
		Repo:       snapshot.target.Repo,
		Token:      token,
		APIBaseURL: githubAPIBaseURL,
	})
	if err != nil {
		return "", publicationRefusal(err)
	}
	if publicationBranch == defaultBranch {
		return "", publicationRefusal(fmt.Errorf("publication branch %q is the remote default branch; inspector may update only a pull request branch", publicationBranch))
	}

	branchRef := "refs/heads/" + publicationBranch
	remoteCommit, exists, err := inspector.RemoteRefCommit(snapshot.repoRoot, snapshot.target.PushURL, branchRef)
	if err != nil {
		return "", publicationRefusal(fmt.Errorf("checking publication branch %q before staging: %w", publicationBranch, err))
	}
	if exists && remoteCommit == result.Commit {
		return "", publicationRefusal(fmt.Errorf("publication branch %q already points at checked commit %s; inspector will not retroactively stamp a commit published before its required staging, status, and branch-update sequence; make the correction as a new local commit and hand that commit to inspector before anything publishes it", publicationBranch, result.Commit))
	}

	stagingRef := inspector.StagingRefForCommit(result.Commit)
	if err := inspector.PushRefToRemote(snapshot.repoRoot, snapshot.target.PushURL, result.Commit, stagingRef); err != nil {
		return "", incompletePublication(fmt.Errorf("staging push failed; whether temporary staging ref %s was updated is unknown, and no status post or branch move was attempted: %w", stagingRef, err))
	}

	description := "inspector: green"
	if result.ReportPath != "" {
		description = fmt.Sprintf("inspector: green - see %s/%s locally", inspector.RunsDirName, inspector.LatestReportName)
	}
	if err := inspector.PostCommitStatus(inspector.PostStatusOptions{
		Owner:       snapshot.target.Owner,
		Repo:        snapshot.target.Repo,
		Commit:      result.Commit,
		State:       inspector.StatusSuccess,
		Description: description,
		Token:       token,
		APIBaseURL:  githubAPIBaseURL,
	}); err != nil {
		return "", incompletePublication(fmt.Errorf("recording the green status did not complete; temporary staging ref %s was created, and branch %q was not attempted: %w", stagingRef, publicationBranch, err))
	}

	if err := inspector.PushRefToRemote(snapshot.repoRoot, snapshot.target.PushURL, result.Commit, branchRef); err != nil {
		return "", incompletePublication(fmt.Errorf("moving branch %q failed after the green status was recorded; whether the remote branch moved is unknown, and temporary staging ref %s may remain: %w", publicationBranch, stagingRef, err))
	}
	if err := inspector.DeleteRefFromRemote(snapshot.repoRoot, snapshot.target.PushURL, stagingRef); err != nil {
		return fmt.Sprintf("inspector published green branch %q, but could not confirm removal of its temporary staging ref: %v", publicationBranch, err), nil
	}
	return "", nil
}

// printPublicationFailure makes an incomplete green publication impossible to
// mistake for a verdict. A green check only becomes green to a GitHub reader
// once all three publication steps have completed.
func printPublicationFailure(stderr io.Writer, err error) {
	var publicationErr *publicationError
	if errors.As(err, &publicationErr) && !publicationErr.attempted {
		fmt.Fprintf(stderr, "refused: %v\nno staging push, status publication, or branch update was attempted.\n", err)
		return
	}
	fmt.Fprintf(stderr, "\n%s\ninspector: could not complete green publication: %v\n"+
		"the local check passed, but the pull-request branch was not confirmed published after the required staging and status steps.\n"+
		"this is an incomplete publication, not a red verdict or a refusal.\n%s\n\n",
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
