// Package examiner independently judges requested observable behavior against a
// running HTTP app without mounting the application's source code.
package examiner

import (
	"errors"
	"fmt"
	"strings"
)

const (
	InputRequestName     = "request.md"
	InputGuidebookName   = "guidebook.md"
	InputTestChangesName = "test-changes.json"
	VerdictName          = "verdict.json"
)

// OutcomeVerdict is the examiner's judgment of one capability it derived from
// the request.
type OutcomeVerdict string

const (
	Confirmed        OutcomeVerdict = "confirmed"
	NotConfirmed     OutcomeVerdict = "not_confirmed"
	CouldNotBeTested OutcomeVerdict = "could_not_be_tested"
)

// Outcome records an independently derived capability and the scenario used to
// operate the app.
type Outcome struct {
	Claim              string         `json:"claim"`
	Verdict            OutcomeVerdict `json:"verdict"`
	Scenario           string         `json:"scenario"`
	Evidence           string         `json:"evidence"`
	ProposedRegression string         `json:"proposedRegression,omitempty"`
}

// Finding is a confirmed weakening of a changed test. It is a proposal for the
// project's own check suite, never an edit to that suite.
type Finding struct {
	TestPath           string `json:"testPath"`
	Detail             string `json:"detail"`
	ProposedRegression string `json:"proposedRegression"`
}

// Verdict is the agent's complete, machine-readable examination result.
type Verdict struct {
	Outcomes []Outcome `json:"outcomes"`
	Findings []Finding `json:"findings,omitempty"`
}

// ResultKind is the overall result after every individual outcome is retained.
type ResultKind string

const (
	Green   ResultKind = "green"
	Red     ResultKind = "red"
	Refused ResultKind = "refused"
)

// Result contains the agent's per-outcome verdict plus its overall result.
type Result struct {
	Kind    ResultKind `json:"kind"`
	Verdict Verdict    `json:"verdict"`
	Message string     `json:"message,omitempty"`
}

// TestChangeList is the narrow evidence channel through which changed tests
// reach the examiner. Fabrica produces it from the task's pinned base commit.
type TestChangeList struct {
	BaseCommit string       `json:"baseCommit"`
	Files      []TestChange `json:"files"`
}

// TestChange describes one test file added, modified, deleted, or renamed.
type TestChange struct {
	Path         string `json:"path"`
	PreviousPath string `json:"previousPath,omitempty"`
	Change       string `json:"change"`
	Before       string `json:"before,omitempty"`
	After        string `json:"after,omitempty"`
}

// Classify verifies the verdict's evidence references and determines its
// overall result. A test weakening or missing capability is red only when the
// judgment finishes. Any outcome that could not be tested makes the whole
// examination a refusal, never a red judgment.
func Classify(v Verdict, tests TestChangeList) (Result, error) {
	if len(v.Outcomes) == 0 {
		return Result{}, errors.New("examiner submitted no outcomes derived from the request")
	}

	testPaths := make(map[string]struct{}, len(tests.Files))
	for _, change := range tests.Files {
		testPaths[change.Path] = struct{}{}
		if change.PreviousPath != "" {
			testPaths[change.PreviousPath] = struct{}{}
		}
	}

	result := Result{Kind: Green, Verdict: v}
	couldNotTest := false
	for n, outcome := range v.Outcomes {
		if strings.TrimSpace(outcome.Claim) == "" {
			return Result{}, fmt.Errorf("outcome %d has no claimed capability", n+1)
		}
		if strings.TrimSpace(outcome.Scenario) == "" {
			return Result{}, fmt.Errorf("outcome %q has no scenario", outcome.Claim)
		}
		if strings.TrimSpace(outcome.Evidence) == "" {
			return Result{}, fmt.Errorf("outcome %q has no evidence", outcome.Claim)
		}
		switch outcome.Verdict {
		case Confirmed:
		case NotConfirmed:
			if strings.TrimSpace(outcome.ProposedRegression) == "" {
				return Result{}, fmt.Errorf("outcome %q is not confirmed but proposes no regression line", outcome.Claim)
			}
			result.Kind = Red
		case CouldNotBeTested:
			couldNotTest = true
		default:
			return Result{}, fmt.Errorf("outcome %q has unknown verdict %q", outcome.Claim, outcome.Verdict)
		}
	}
	for n, finding := range v.Findings {
		if _, ok := testPaths[finding.TestPath]; !ok {
			return Result{}, fmt.Errorf("test-change finding %d names %q, which is not in the supplied test-change list", n+1, finding.TestPath)
		}
		if strings.TrimSpace(finding.Detail) == "" {
			return Result{}, fmt.Errorf("test-change finding for %q does not say what it stopped checking", finding.TestPath)
		}
		if strings.TrimSpace(finding.ProposedRegression) == "" {
			return Result{}, fmt.Errorf("test-change finding for %q proposes no regression line", finding.TestPath)
		}
		result.Kind = Red
	}
	if couldNotTest {
		result.Kind = Refused
		result.Message = "one or more claimed capabilities could not be tested; this is not a finding that the work is wrong"
	}
	return result, nil
}
