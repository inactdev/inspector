// Package reviewer independently reads a change and reports what looks
// wrong, pointing at the exact file and line. It never runs the reviewed
// application and never edits the reviewed project - see SPEC.md's
// "The reviewer".
package reviewer

import (
	"errors"
	"fmt"
	"strings"
)

// Finding is one thing that looked wrong in the change. It is a report, not
// a merge-blocking verdict: every finding travels with the delivery and the
// Client rules on it, the same as every other inspector finding.
type Finding struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Summary string `json:"summary"`
}

// ResultKind is the overall outcome of one review run.
type ResultKind string

const (
	// Reviewed means the model read the change and reported, including when
	// it reported nothing - a clean review is a legitimate result, not an
	// incomplete one.
	Reviewed ResultKind = "reviewed"
	// Refused means the review could not be completed at all - missing
	// configuration, an unreadable diff, or a model failure.
	Refused ResultKind = "refused"
)

// Result is the record-derived outcome of one review run.
type Result struct {
	Kind     ResultKind `json:"kind"`
	Findings []Finding  `json:"findings,omitempty"`
	Message  string     `json:"message,omitempty"`
	// Vendor names which model vendor produced this result, e.g. "anthropic
	// (default, no flag given)" or "openrouter" - set by the caller so the
	// choice is never silent, matching the examiner's own report.
	Vendor string `json:"vendor,omitempty"`
	// TestChangeVerdict is inspector's own derived answer about the
	// change's test files (issue #44). Every completed review carries one,
	// including a run given no test-path patterns, so a run that judged
	// nothing never reads as a run that found nothing to judge. It is
	// absent only on a Refused result, which judged nothing at all.
	TestChangeVerdict TestChangeVerdict `json:"testChangeVerdict,omitempty"`
	// TestChanges is every changed test file the verdict was derived from,
	// carrying the diff-derived facts and the model's judgment of each.
	TestChanges []TestChange `json:"testChanges,omitempty"`
}

// Record owns every finding and test-change judgment the model reports for
// one run and validates each one against the change actually under review
// before accepting it.
type Record struct {
	paths    map[string]struct{}
	Findings []Finding
	// TestChanges is seeded from the diff before the model is asked
	// anything, so the set of files to judge is inspector's and the model
	// can neither add to it nor drop from it.
	TestChanges []TestChange
}

// NewRecord starts a record that only accepts findings naming one of paths -
// the files that actually appear in the reviewed change - and test-change
// judgments naming one of testChanges.
func NewRecord(paths map[string]struct{}, testChanges []TestChange) *Record {
	return &Record{paths: paths, TestChanges: testChanges}
}

// AddFindings validates and appends a batch of findings atomically: a batch
// containing one invalid finding is rejected in full, so a partially invalid
// report is never split into an accepted prefix and a rejected remainder.
func (r *Record) AddFindings(findings []Finding) error {
	if len(findings) == 0 {
		return errors.New("no findings were given")
	}
	for _, finding := range findings {
		if strings.TrimSpace(finding.Path) == "" {
			return errors.New("a finding has no path")
		}
		if _, ok := r.paths[finding.Path]; !ok {
			return fmt.Errorf("finding names %q, which is not a file in the reviewed change", finding.Path)
		}
		if finding.Line <= 0 {
			return fmt.Errorf("finding for %q has no line pointer", finding.Path)
		}
		if strings.TrimSpace(finding.Summary) == "" {
			return fmt.Errorf("finding for %q has no summary of what looks wrong", finding.Path)
		}
	}
	r.Findings = append(r.Findings, findings...)
	return nil
}

// JudgeTestChange records the model's judgment of one changed test file. A
// judgment naming something that is not a changed test file in this change
// is rejected by name, and a second judgment for the same path is rejected
// rather than replacing the first - otherwise a weakened verdict could be
// written over with an intact one.
func (r *Record) JudgeTestChange(path string, judgment TestJudgment, reason string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("a test-change judgment has no path")
	}
	switch judgment {
	case ExamWeakened, ExamIntact, ExamUnclear:
	default:
		return fmt.Errorf("judgment %q for %q is not one of %q, %q, %q", judgment, path, ExamWeakened, ExamIntact, ExamUnclear)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("judgment for %q has no reason", path)
	}
	for i := range r.TestChanges {
		if r.TestChanges[i].Path != path {
			continue
		}
		if r.TestChanges[i].Judged() {
			return fmt.Errorf("%q has already been judged %q; one judgment per changed test file", path, r.TestChanges[i].Judgment)
		}
		r.TestChanges[i].Judgment = judgment
		r.TestChanges[i].Reason = reason
		return nil
	}
	return fmt.Errorf("%q is not a changed test file in the reviewed change", path)
}

// UnjudgedTestPaths names every changed test file still carrying no
// judgment. The review cannot finish while this is non-empty.
func (r *Record) UnjudgedTestPaths() []string {
	var paths []string
	for _, change := range r.TestChanges {
		if !change.Judged() {
			paths = append(paths, change.Path)
		}
	}
	return paths
}
