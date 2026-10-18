package main

import (
	"context"
	"fmt"
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
		return testChangeFailure(judgment, fmt.Sprintf("%s changed, and no --model was given to judge %s with, so whether the exam survived is unknown", changedTestList(changes), pluralTests(len(changes))))
	}

	result, err := judgeTestChanges(context.Background(), reviewer.AgentOptions{
		Diff:             diff,
		TestPathPatterns: patterns,
		Vendor:           judgment.Vendor,
		Model:            judgment.Model,
	})
	if err != nil {
		return testChangeFailure(judgment, fmt.Sprintf("%s changed, and the judgment of %s could not be completed: %v", changedTestList(changes), pluralTests(len(changes)), err))
	}

	switch result.TestChangeVerdict {
	case reviewer.TestChangesIntact:
		return testJudgment{
			Posted:      true,
			State:       inspector.StatusSuccess,
			Description: testChangeDescription(judgment, fmt.Sprintf("%s intact: %s", pluralTests(len(result.TestChanges)), changedTestList(result.TestChanges))),
			Summary:     fmt.Sprintf("every changed test keeps its exam intact (%s)", changedTestList(result.TestChanges)),
		}
	case reviewer.TestChangesWeaken:
		return testChangeFailure(judgment, weakenedTestReason(result.TestChanges))
	default:
		// Unjudged, and anything the reviewer could in principle grow later:
		// neither reads as intact, so neither clears the gate.
		return testChangeFailure(judgment, unjudgedTestReason(result))
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
