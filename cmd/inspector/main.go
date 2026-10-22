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
	"github.com/inactdev/inspector/internal/llm"
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
	if len(args) > 0 {
		switch args[0] {
		case "examine":
			return runExaminer(args[1:], stdout, stderr)
		case "review":
			return runReviewer(args[1:], stdout, stderr)
		}
	}
	fs := flag.NewFlagSet("inspector", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoPath := fs.String("repo", ".", "path to the repository to inspect")
	publicationBranch := fs.String("branch", "", "pull request branch to publish after a green result (required)")
	expectedTip := fs.String("expected-tip", "", "full commit SHA the pull request branch must still point at on the remote; given, the branch update may replace a history it does not contain (a rebase), and any other remote commit is refused without pushing. Omitted, the update stays fast-forward only")
	prTitle := fs.String("pr-title", "", "pull request title to open after a green result (requires -pr-body-file; both or neither)")
	prBodyFile := fs.String("pr-body-file", "", "path to a file containing the pull request body (requires -pr-title; both or neither)")
	vendor := fs.String("vendor", "", "model vendor for judging changed test files (default: anthropic); reads <VENDOR>_API_KEY and, for every vendor except anthropic, <VENDOR>_BASE_URL from the environment")
	model := fs.String("model", "", "model name for judging changed test files; required only when this project declares testPaths in .inspector.json")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `usage: inspector [flags] [claim text...]

Runs the project's own check command against the repo's current HEAD
commit and reports green or red. --branch must explicitly name the pull
request branch that inspector may update after a green result. Inspector
puts the commit on GitHub, records its status, then updates only that named
branch, so it needs GITHUB_TOKEN set to a token with commit-status write
access and a GitHub 'origin' remote. See README.md for setup.

-pr-title and -pr-body-file, given together, open a pull request for that
branch against the repository's default branch once publication completes;
given alone, either one is a usage error. Given neither, no pull request is
opened.

The branch update is fast-forward only, so a branch whose history was
rewritten locally - a rebase - cannot be published without saying so.
-expected-tip <sha> is how a caller says so: it names the full commit SHA the
branch must still point at on the remote, and the update then carries a lease
on exactly that commit. A remote sitting on any other commit is refused with
nothing pushed, because an unexpected commit there is work nobody accounted
for.

When the project declares testPaths in .inspector.json and this commit's
change touches one of those files, inspector also judges whether any changed
or removed test undercuts what the request asked for, and posts that as a
separate "inspector-tests" commit status - success only when every changed
test is confirmed intact. That judgment needs --model (and optionally
--vendor); without one it posts a failure saying so, which is what
inspector-gate reads before letting a changed test file through.

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
	if *expectedTip != "" {
		if err := inspector.ValidateExpectedTip(*expectedTip); err != nil {
			fmt.Fprintf(stderr, "refused: invalid --expected-tip: %v\n", err)
			return exitRefused
		}
	}
	branchTarget := publicationBranchTarget{Branch: *publicationBranch, ExpectedTip: *expectedTip}
	prTitleSet := strings.TrimSpace(*prTitle) != ""
	prBodyFileSet := strings.TrimSpace(*prBodyFile) != ""
	openPullRequest := prTitleSet && prBodyFileSet
	var prBody string
	switch {
	case prTitleSet && !prBodyFileSet:
		fmt.Fprintln(stderr, "refused: -pr-title was given without -pr-body-file; both are required to open a pull request, or neither to skip it")
		return exitRefused
	case prBodyFileSet && !prTitleSet:
		fmt.Fprintln(stderr, "refused: -pr-body-file was given without -pr-title; both are required to open a pull request, or neither to skip it")
		return exitRefused
	case openPullRequest:
		content, err := os.ReadFile(*prBodyFile)
		if err != nil {
			fmt.Fprintf(stderr, "refused: reading -pr-body-file %s: %v\n", *prBodyFile, err)
			return exitRefused
		}
		prBody = string(content)
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

	judgment := testJudgmentOptions{Vendor: *vendor, Model: *model, VendorDescription: llm.Describe(*vendor)}

	switch result.Outcome {
	case inspector.Green:
		cleanupWarning, tests, err := publishGreen(publicationTarget, branchTarget, result, judgment)
		// Before the error is handled: the judgment runs before the first
		// remote step, so a publication that then failed still has a review
		// worth keeping in the report.
		recordTestReview(stderr, publicationTarget.repoRoot, result.ReportPath, tests)
		if err != nil {
			printPublicationFailure(stderr, err)
			return exitRefused
		}
		if cleanupWarning != "" {
			fmt.Fprintf(stderr, "\n%s\n%s\n%s\n\n", warningBar, cleanupWarning, warningBar)
		}
		fmt.Fprintf(stdout, "\ngreen - %s%s\n", result.Commit, reportSuffix(result.ReportPath))
		printTestJudgment(stdout, stderr, tests)
		if openPullRequest {
			openGreenPullRequest(publicationTarget, *publicationBranch, *prTitle, prBody, result, stdout, stderr)
		}
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

// publicationBranchTarget names the branch inspector may move and, when the
// caller rewrote that branch's history locally, the remote commit it must
// still find there. The two travel together because the second only ever
// authorizes a rewrite of the first: an empty ExpectedTip leaves the branch
// update fast-forward only, exactly as it has always been.
type publicationBranchTarget struct {
	Branch      string
	ExpectedTip string
}

// publishGreen performs the publication order that keeps inspector-gate from
// observing a pull-request branch without an inspector status: stage the
// checked commit remotely, record its green status, record the changed-test
// judgment beside it, then move the branch.
// The stage is a non-branch ref; its deletion is attempted once the branch moves.
//
// The test judgment is computed before any remote step and posted after the
// green status but before the branch moves, so the gate's very first look at
// the pull request already sees both statuses - the same reason the green
// status itself precedes the branch move.
func publishGreen(snapshot publicationTargetSnapshot, branchTarget publicationBranchTarget, result inspector.Result, judgment testJudgmentOptions) (cleanupWarning string, tests testJudgment, err error) {
	publicationBranch := branchTarget.Branch
	if err := validatePublicationPlatform(); err != nil {
		return "", testJudgment{}, publicationRefusal(err)
	}
	if err := inspector.ValidatePublicationBranch(publicationBranch); err != nil {
		return "", testJudgment{}, publicationRefusal(err)
	}
	if branchTarget.ExpectedTip != "" {
		if err := inspector.ValidateExpectedTip(branchTarget.ExpectedTip); err != nil {
			return "", testJudgment{}, publicationRefusal(err)
		}
	}
	token := os.Getenv(inspector.GitHubTokenEnvVar)
	if strings.TrimSpace(token) == "" {
		return "", testJudgment{}, publicationRefusal(fmt.Errorf("%s is not set - inspector needs a GitHub token with commit-status write access to publish a green result", inspector.GitHubTokenEnvVar))
	}

	if snapshot.targetErr != nil {
		return "", testJudgment{}, publicationRefusal(snapshot.targetErr)
	}
	if snapshot.validationError != nil {
		return "", testJudgment{}, publicationRefusal(snapshot.validationError)
	}
	if err := inspector.ValidatePublicationCommit(snapshot.repoRoot, result.Commit, snapshot.validation); err != nil {
		return "", testJudgment{}, publicationRefusal(err)
	}
	currentTarget, err := resolvePublicationTarget(snapshot.repoRoot)
	if err != nil {
		return "", testJudgment{}, publicationRefusal(fmt.Errorf("publication target changed during inspection: %w", err))
	}
	if currentTarget != snapshot.target {
		return "", testJudgment{}, publicationRefusal(fmt.Errorf("publication target changed during inspection; refusing to publish to either destination"))
	}
	defaultBranch, err := inspector.RepositoryDefaultBranch(inspector.RepositoryOptions{
		Owner:      snapshot.target.Owner,
		Repo:       snapshot.target.Repo,
		Token:      token,
		APIBaseURL: githubAPIBaseURL,
	})
	if err != nil {
		return "", testJudgment{}, publicationRefusal(err)
	}
	if publicationBranch == defaultBranch {
		return "", testJudgment{}, publicationRefusal(fmt.Errorf("publication branch %q is the remote default branch; inspector may update only a pull request branch", publicationBranch))
	}

	branchRef := "refs/heads/" + publicationBranch
	remoteCommit, exists, err := inspector.RemoteRefCommit(snapshot.repoRoot, snapshot.target.PushURL, branchRef)
	if err != nil {
		return "", testJudgment{}, publicationRefusal(fmt.Errorf("checking publication branch %q before staging: %w", publicationBranch, err))
	}
	if exists && remoteCommit == result.Commit {
		return "", testJudgment{}, publicationRefusal(fmt.Errorf("publication branch %q already points at checked commit %s; inspector will not retroactively stamp a commit published before its required staging, status, and branch-update sequence; make the correction as a new local commit and hand that commit to inspector before anything publishes it", publicationBranch, result.Commit))
	}
	// Read before the first remote write, so a remote the caller did not
	// account for costs nothing and is reported as a refusal rather than a
	// half-finished publication. The lease on the push itself then re-decides
	// the same question at the remote, closing the gap between this read and
	// that push.
	if branchTarget.ExpectedTip != "" {
		if !exists {
			return "", testJudgment{}, publicationRefusal(fmt.Errorf("publication branch %q does not exist on the remote, but --expected-tip names %s; inspector replaces a branch's history only where the remote still shows the exact commit the caller accounted for, so nothing was pushed and the branch was not created", publicationBranch, branchTarget.ExpectedTip))
		}
		if remoteCommit != branchTarget.ExpectedTip {
			return "", testJudgment{}, publicationRefusal(fmt.Errorf("publication branch %q is at %s on the remote, not the --expected-tip %s given; inspector will not replace a history the caller did not account for, so nothing was pushed and no status was recorded; read the branch's current remote tip, rebase onto it if that work must be kept, and hand the result back", publicationBranch, remoteCommit, branchTarget.ExpectedTip))
		}
	}

	// Judged before the first remote step, so a slow or failing model leaves
	// no half-published state behind. It can only produce a status, never a
	// refusal - see judgeChangedTests.
	tests = judgeChangedTests(snapshot.repoRoot, result.Commit, defaultBranch, result.TestPaths, judgment)

	stagingRef := inspector.StagingRefForCommit(result.Commit)
	if err := inspector.PushRefToRemote(snapshot.repoRoot, snapshot.target.PushURL, result.Commit, stagingRef); err != nil {
		return "", tests, incompletePublication(fmt.Errorf("staging push failed; whether temporary staging ref %s was updated is unknown, and no status post or branch move was attempted: %w", stagingRef, err))
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
		return "", tests, incompletePublication(fmt.Errorf("recording the green status did not complete; temporary staging ref %s was created, and branch %q was not attempted: %w", stagingRef, publicationBranch, err))
	}

	if tests.Posted {
		if err := inspector.PostCommitStatus(inspector.PostStatusOptions{
			Owner:       snapshot.target.Owner,
			Repo:        snapshot.target.Repo,
			Commit:      result.Commit,
			State:       tests.State,
			Context:     inspector.TestsStatusContext,
			Description: tests.Description,
			Token:       token,
			APIBaseURL:  githubAPIBaseURL,
		}); err != nil {
			return "", tests, incompletePublication(fmt.Errorf("recording the %q status did not complete; the green status was recorded, temporary staging ref %s was created, and branch %q was not attempted: %w", inspector.TestsStatusContext, stagingRef, publicationBranch, err))
		}
	}

	if err := updatePublicationBranch(snapshot, branchTarget, branchRef, result.Commit); err != nil {
		if errors.Is(err, inspector.ErrStaleLease) {
			return "", tests, incompletePublication(fmt.Errorf("branch %q was not moved: the remote had left --expected-tip %s by the time the push ran, so it refused to replace that history. the green status was recorded and temporary staging ref %s may remain: %w", publicationBranch, branchTarget.ExpectedTip, stagingRef, err))
		}
		return "", tests, incompletePublication(fmt.Errorf("moving branch %q failed after the green status was recorded; whether the remote branch moved is unknown, and temporary staging ref %s may remain: %w", publicationBranch, stagingRef, err))
	}
	if err := inspector.DeleteRefFromRemote(snapshot.repoRoot, snapshot.target.PushURL, stagingRef); err != nil {
		return fmt.Sprintf("inspector published green branch %q, but could not confirm removal of its temporary staging ref: %v", publicationBranch, err), tests, nil
	}
	return "", tests, nil
}

// updatePublicationBranch is the last remote write of the sequence. Without
// --expected-tip it is the fast-forward-only push it has always been; with it,
// the push carries a lease on that exact commit, so the remote - not
// inspector's own earlier read of it - is what finally decides whether the
// history being replaced is the one the caller accounted for.
func updatePublicationBranch(snapshot publicationTargetSnapshot, branchTarget publicationBranchTarget, branchRef, commit string) error {
	if branchTarget.ExpectedTip != "" {
		return inspector.ForcePushRefWithLease(snapshot.repoRoot, snapshot.target.PushURL, commit, branchRef, branchTarget.ExpectedTip)
	}
	return inspector.PushRefToRemote(snapshot.repoRoot, snapshot.target.PushURL, commit, branchRef)
}

// printTestJudgment reports the changed-test status on stdout and, for
// anything other than success, repeats it loudly on stderr. A green run that
// posted a failing inspector-tests status is still a run the gate will hold
// back, so it must not be possible to read the "green" line alone and think
// the pull request is through.
func printTestJudgment(stdout, stderr io.Writer, tests testJudgment) {
	if !tests.Posted {
		return
	}
	fmt.Fprintf(stdout, "%s: %s - %s\n", inspector.TestsStatusContext, tests.State, tests.Summary)
	printTestReview(stdout, tests.Review)
	if tests.State != inspector.StatusSuccess {
		fmt.Fprintf(stderr, "\n%s\n%s\n%s\n\n", warningBar, tests.Warning, warningBar)
	}
}

// printTestReview prints who judged the changed tests and what they concluded
// about each one, on a pass exactly as on a failure. The status description is
// capped at 140 characters and the summary line above names at most one file,
// so without this a caller reading only stdout never learns what the reviewer
// concluded about any particular test (issue #55).
func printTestReview(stdout io.Writer, review inspector.TestReview) {
	fmt.Fprintf(stdout, "  judged by vendor: %s, model: %s\n", orNoneGiven(review.Vendor), orNoneGiven(review.Model))
	for _, file := range review.Files {
		judgment := file.Judgment
		if judgment == "" {
			judgment = "unjudged"
		}
		fmt.Fprintf(stdout, "  %s (%s): %s", file.Path, file.Status, judgment)
		if file.Reason != "" {
			fmt.Fprintf(stdout, " - %s", file.Reason)
		}
		fmt.Fprintln(stdout)
	}
}

// orNoneGiven keeps a vendor or model nobody named from printing as an empty
// gap, which reads as a broken line rather than as an absence.
func orNoneGiven(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(none given)"
	}
	return value
}

// openGreenPullRequest runs once publishGreen has already completed in full
// - the branch is pushed and its status is stamped. Opening the pull request
// is therefore never part of the staging/status/branch sequence itself and
// never changes the green verdict or exit code: a failure here is reported
// loudly on stderr as its own warning, distinct from an incomplete
// publication, because the check and its publication already succeeded.
// It looks for an existing open pull request first, so a second green run on
// the same branch reports that one instead of opening a second.
func openGreenPullRequest(snapshot publicationTargetSnapshot, publicationBranch, title, body string, result inspector.Result, stdout, stderr io.Writer) {
	token := os.Getenv(inspector.GitHubTokenEnvVar)
	defaultBranch, err := inspector.RepositoryDefaultBranch(inspector.RepositoryOptions{
		Owner:      snapshot.target.Owner,
		Repo:       snapshot.target.Repo,
		Token:      token,
		APIBaseURL: githubAPIBaseURL,
	})
	if err != nil {
		printPullRequestFailure(stderr, fmt.Errorf("resolving the default branch to open the pull request against: %w", err))
		return
	}
	prOpts := inspector.PullRequestOptions{
		Owner:      snapshot.target.Owner,
		Repo:       snapshot.target.Repo,
		Head:       publicationBranch,
		Base:       defaultBranch,
		Title:      title,
		Body:       body,
		Token:      token,
		APIBaseURL: githubAPIBaseURL,
	}
	url, found, err := inspector.FindExistingPullRequest(prOpts)
	if err != nil {
		printPullRequestFailure(stderr, fmt.Errorf("checking for an existing pull request: %w", err))
		return
	}
	if !found {
		url, err = inspector.OpenPullRequest(prOpts)
		if err != nil {
			printPullRequestFailure(stderr, fmt.Errorf("opening the pull request: %w", err))
			return
		}
	}
	fmt.Fprintf(stdout, "pull request: %s\n", url)
	if err := inspector.RecordPullRequestURL(snapshot.repoRoot, result.ReportPath, url); err != nil {
		fmt.Fprintf(stderr, "warning: could not record the pull request URL in the local report: %v\n", err)
	}
}

// printPullRequestFailure is deliberately distinct from printPublicationFailure:
// the staging, status, and branch sequence already completed, so this is never
// an incomplete publication or a refusal, just a loud, separate warning.
func printPullRequestFailure(stderr io.Writer, err error) {
	fmt.Fprintf(stderr, "\n%s\ninspector: could not open the pull request: %v\n"+
		"the check passed and the branch was already published in full; only the pull request could not be confirmed.\n%s\n\n",
		warningBar, err, warningBar)
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
