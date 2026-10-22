package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/inactdev/inspector/internal/inspector"
	"github.com/inactdev/inspector/internal/reviewer"
)

// judgeTestChanges is the reviewer's own test-change judgment, reached
// through the same vendor seam and the same environment-read key
// `inspector review` uses. Replaced in tests so the publication sequence can
// be exercised without a model.
var judgeTestChanges = reviewer.RunAgent

// testJudgmentOptions is what `inspector run` was told about the model to
// judge with. Both fields come from flags; neither is ever a secret - the API
// key is read from the environment by name inside internal/llm.
type testJudgmentOptions struct {
	Vendor            string
	Model             string
	VendorDescription string
}

// testJudgment is the outcome of judging the checked commit's changed test
// files, in the shape publication needs it: whether a status is owed at all,
// and if so which state and what to say.
type testJudgment struct {
	// Posted is false only when nothing is owed - the project declared no
	// testPaths, or the change touched no file matching them. Those two are
	// the one case where inspector stays silent, exactly as it did before
	// this status existed.
	Posted bool
	State  inspector.StatusState
	// Description is the status text; Summary is the one stdout line.
	Description string
	Summary     string
	// Review is the per-file evidence behind this status: which changed
	// test files the reviewer covered, what it judged each one, and why,
	// plus who judged them. GitHub caps the status description at 140
	// characters, so this is the only place those reasons can be read -
	// printed on stdout and recorded in the run report, on a pass exactly
	// as on a failure (issue #55).
	Review inspector.TestReview
	// Warning is set for any state other than success: the text printed
	// loudly on stderr, because the worker reading it is the one who has to
	// put the exam back.
	Warning string
}

// judgeChangedTests answers the one question issue #44 asks of `inspector
// run`: does any changed or removed test undercut the behavior the request
// asked for? It is deliberately incapable of failing the run - every way it
// can go wrong becomes a failure STATUS, never a refusal, because a refusal
// would suppress the publication the gate needs in order to see any status at
// all, and an unpublished green with no inspector-tests status is read by the
// gate as red for every changed test anyway. Blocking lives in the status and
// the gate, the same split the examiner uses.
func judgeChangedTests(repoRoot, commit, baseBranch string, patterns []string, judgment testJudgmentOptions) testJudgment {
	if len(patterns) == 0 {
		// The project never said which of its files are tests, so inspector
		// has nothing to judge and says nothing. It must not post success
		// here: that would hand every project a status clearing protected
		// test files it never asked to have judged.
		return testJudgment{}
	}
	baseRef := inspector.RemoteTrackingRef(baseBranch)
	diff, err := inspector.CommitDiffAgainstBase(repoRoot, commit, baseRef)
	if err != nil {
		return testChangeFailure(judgment, fmt.Sprintf("the change this commit proposes against %s could not be read, so whether it touches a test file is unknown: %v", baseRef, err))
	}
	files, err := reviewer.ParseUnifiedDiff(diff)
	if err != nil {
		return testChangeFailure(judgment, fmt.Sprintf("the change this commit proposes could not be parsed, so its test files could not be found: %v", err))
	}
	changes, err := reviewer.ChangedTests(files, patterns)
	if err != nil {
		return testChangeFailure(judgment, fmt.Sprintf("this project's testPaths patterns could not be used: %v", err))
	}
	if len(changes) == 0 {
		return testJudgment{}
	}
	if strings.TrimSpace(judgment.Model) == "" {
		return testChangeFailure(judgment, fmt.Sprintf("%s changed, and no --model was given to judge %s with, so whether the exam survived is unknown", changedTestList(changes), pluralTests(len(changes)))).
			withReviewedTests(reviewer.DeriveTestChangeVerdict(true, changes), changes)
	}

	result, err := judgeTestChanges(context.Background(), reviewer.AgentOptions{
		Diff:             diff,
		TestPathPatterns: patterns,
		Vendor:           judgment.Vendor,
		Model:            judgment.Model,
	})
	if err != nil {
		return testChangeFailure(judgment, fmt.Sprintf("%s changed, and the judgment of %s could not be completed: %v", changedTestList(changes), pluralTests(len(changes)), err)).
			withReviewedTests(reviewer.DeriveTestChangeVerdict(true, changes), changes)
	}

	switch result.TestChangeVerdict {
	case reviewer.TestChangesIntact:
		summary := fmt.Sprintf("every changed test keeps its exam intact (%s)", changedTestList(result.TestChanges))
		return testJudgment{
			Posted:      true,
			State:       inspector.StatusSuccess,
			Description: testChangeDescription(judgment, fmt.Sprintf("%s intact: %s", pluralTests(len(result.TestChanges)), changedTestList(result.TestChanges))),
			Summary:     summary,
			Review:      newTestReview(judgment, inspector.StatusSuccess, summary),
		}.withReviewedTests(result.TestChangeVerdict, result.TestChanges)
	case reviewer.TestChangesWeaken:
		return testChangeFailure(judgment, weakenedTestReason(result.TestChanges)).
			withReviewedTests(result.TestChangeVerdict, result.TestChanges)
	default:
		// Unjudged, and anything the reviewer could in principle grow later:
		// neither reads as intact, so neither clears the gate.
		return testChangeFailure(judgment, unjudgedTestReason(result)).
			withReviewedTests(result.TestChangeVerdict, result.TestChanges)
	}
}

// recordTestReview writes the review's per-file judgments into this run's
// report, so the reasons behind the inspector-tests status outlive the
// 140-character status description and reach the file Fabrica copies onto the
// task record. It runs whether the publication that followed the judgment
// completed or not - the judgment happens before the first remote step, so
// its evidence is worth keeping either way - and a failure to write it is a
// warning and nothing more, the same rule a failed report save follows: the
// report is notes, never authority.
func recordTestReview(stderr io.Writer, repoRoot, reportPath string, tests testJudgment) {
	if !tests.Posted {
		return
	}
	if err := inspector.RecordTestReview(repoRoot, reportPath, tests.Review); err != nil {
		fmt.Fprintf(stderr, "warning: could not record the changed-test review in the local report: %v\n", err)
	}
}

// testChangeFailure is the one constructor for every non-success outcome, so
// a reason can never be recorded without also being posted as a failure.
func testChangeFailure(judgment testJudgmentOptions, reason string) testJudgment {
	return testJudgment{
		Posted:      true,
		State:       inspector.StatusFailure,
		Description: testChangeDescription(judgment, reason),
		Summary:     reason,
		Review:      newTestReview(judgment, inspector.StatusFailure, reason),
		Warning: fmt.Sprintf(
			"the changed tests in this commit were not confirmed to keep their exam intact:\n\n  %s\n\n"+
				"inspector published the green check status, and posted %q as a failure on the same commit, so\n"+
				"inspector-gate will keep this pull request's changed test files red rather than letting them\n"+
				"through. Restore what the test was proving, or hand the reason to the Client for his own override.",
			reason, inspector.TestsStatusContext),
	}
}

// testChangeDescription names the vendor in the status itself, the same way
// the examiner's does - the choice of who judged must never be silent.
func testChangeDescription(judgment testJudgmentOptions, detail string) string {
	return statusDescription(fmt.Sprintf("vendor: %s; %s: %s", judgment.VendorDescription, inspector.TestsStatusContext, detail))
}

// newTestReview starts the review record every outcome carries, naming who
// judged before anything is known about what they concluded - so an outcome
// reached before the model ever ran still records which model it would have
// been.
func newTestReview(judgment testJudgmentOptions, state inspector.StatusState, summary string) inspector.TestReview {
	return inspector.TestReview{
		Vendor:  judgment.VendorDescription,
		Model:   judgment.Model,
		State:   state,
		Summary: summary,
	}
}

// withReviewedTests attaches the changed test files this outcome was derived
// from, with whatever each one was judged. A file the reviewer never reached
// keeps an empty judgment and is still listed: the report has to be able to
// say that nobody judged it, which is not the same as there being nothing to
// judge.
func (t testJudgment) withReviewedTests(verdict reviewer.TestChangeVerdict, changes []reviewer.TestChange) testJudgment {
	t.Review.Verdict = string(verdict)
	t.Review.Files = testReviewFiles(changes)
	return t
}

func testReviewFiles(changes []reviewer.TestChange) []inspector.TestReviewFile {
	files := make([]inspector.TestReviewFile, 0, len(changes))
	for _, change := range changes {
		files = append(files, inspector.TestReviewFile{
			Path:     change.Path,
			Status:   string(change.Status),
			Judgment: string(change.Judgment),
			Reason:   change.Reason,
		})
	}
	return files
}

func weakenedTestReason(changes []reviewer.TestChange) string {
	for _, change := range changes {
		if change.Judgment == reviewer.ExamWeakened {
			return fmt.Sprintf("%s (%s) weakens the exam: %s", change.Path, change.Status, change.Reason)
		}
	}
	return "a changed test weakens the exam"
}

func unjudgedTestReason(result reviewer.Result) string {
	for _, change := range result.TestChanges {
		if !change.Judged() {
			return fmt.Sprintf("%s (%s) carries no judgment, so whether the exam survived is unknown", change.Path, change.Status)
		}
		if change.Judgment == reviewer.ExamUnclear {
			return fmt.Sprintf("%s (%s) could not be judged from the change alone: %s", change.Path, change.Status, change.Reason)
		}
	}
	return fmt.Sprintf("the changed tests were not confirmed intact (%s)", result.TestChangeVerdict)
}

func changedTestList(changes []reviewer.TestChange) string {
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	return strings.Join(paths, ", ")
}

func pluralTests(n int) string {
	if n == 1 {
		return "the changed test"
	}
	return "the changed tests"
}
