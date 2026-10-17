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
}

// Record owns every finding the model reports for one run and validates each
// one against the change actually under review before accepting it.
type Record struct {
	paths    map[string]struct{}
	Findings []Finding
}

// NewRecord starts a record that only accepts findings naming one of paths -
// the files that actually appear in the reviewed change.
func NewRecord(paths map[string]struct{}) *Record {
	return &Record{paths: paths}
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
