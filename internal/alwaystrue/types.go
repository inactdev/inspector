package alwaystrue

import (
	"fmt"
	"sort"
	"strings"
)

// Kind is check 3's aggregate verdict.
type Kind string

const (
	// Green means every promise was driven and every one of them held.
	Green Kind = "green"
	// Red means at least one promise broke. It outranks incompleteness, the
	// same precedence the examiner gives a confirmed bug: a broken promise
	// must not be hidden by what could not be checked, and what could not be
	// checked must not be hidden by a broken promise, so both are reported.
	Red Kind = "red"
	// Refused means the run never got as far as driving the list.
	Refused Kind = "refused"
)

// Outcome is one line's own result.
type Outcome string

const (
	// Held means the falsifier ran and did not falsify the promise.
	Held Outcome = "held"
	// Broken means the falsifier ran and the promise did not survive it.
	Broken Outcome = "broken"
	// NotRunnable means the line declares no falsifier, so there is nothing
	// deterministic to run. Never a pass.
	NotRunnable Outcome = "not_runnable"
	// NotRun means the line has a falsifier that was never attempted,
	// because the app it needed never came up or the run was cut short.
	NotRun Outcome = "not_run"
)

// PromiseResult is what check 3 observed about one line.
type PromiseResult struct {
	ID        string  `json:"id"`
	Promise   string  `json:"promise"`
	Confirmed bool    `json:"confirmed"`
	Shape     string  `json:"shape"`
	Outcome   Outcome `json:"outcome"`
	// Reason says what happened in one line, for the status and the summary.
	Reason string `json:"reason,omitempty"`
	// Evidence is what was actually observed - the response, the exit status,
	// the output tail. The verdict is derived from this, never from the
	// absence of a complaint.
	Evidence string `json:"evidence,omitempty"`
}

// Result is check 3's whole record for one run.
type Result struct {
	Kind     Kind   `json:"kind"`
	ListPath string `json:"listPath,omitempty"`
	// Message carries a refusal's reason.
	Message string `json:"message,omitempty"`
	// Incomplete is true when something could not be checked, whether or not
	// a promise also broke. Kept separate from Kind for the same reason the
	// examiner keeps examinationIncomplete separate from its verdict.
	Incomplete bool `json:"incomplete"`
	// Infrastructure names a failure that is not a promise breaking - the app
	// would not start, its image is missing, a falsifier container could not
	// be launched.
	Infrastructure string          `json:"infrastructure,omitempty"`
	AppStarted     bool            `json:"appStarted"`
	Promises       []PromiseResult `json:"promises,omitempty"`
}

// DeriveResult decides the aggregate from the per-line record alone. Nothing
// else classifies: a run that drove nothing cannot report that everything
// still holds, and a line that was never run cannot report as one that held.
func DeriveResult(listPath string, appStarted bool, infrastructure string, promises []PromiseResult) Result {
	result := Result{
		Kind:           Green,
		ListPath:       listPath,
		AppStarted:     appStarted,
		Infrastructure: infrastructure,
		Promises:       promises,
	}
	for _, promise := range promises {
		switch promise.Outcome {
		case Broken:
			result.Kind = Red
		case NotRunnable, NotRun:
			result.Incomplete = true
		case Held:
		default:
			// An outcome nobody set is not a pass.
			result.Incomplete = true
		}
	}
	if infrastructure != "" {
		result.Incomplete = true
	}
	return result
}

// RefusedResult is the record for a run that never reached the list.
func RefusedResult(listPath, message string) Result {
	return Result{Kind: Refused, ListPath: listPath, Message: message, Incomplete: true}
}

// BrokenPromises returns every promise that broke, in list order.
func (r Result) BrokenPromises() []PromiseResult {
	return r.withOutcome(Broken)
}

// UncheckedPromises returns every promise check 3 could not decide, in list
// order - the not-runnable lines first, as they are the ones a person acts on.
func (r Result) UncheckedPromises() []PromiseResult {
	return append(r.withOutcome(NotRunnable), r.withOutcome(NotRun)...)
}

func (r Result) withOutcome(outcome Outcome) []PromiseResult {
	var matched []PromiseResult
	for _, promise := range r.Promises {
		if promise.Outcome == outcome {
			matched = append(matched, promise)
		}
	}
	return matched
}

// Unconfirmed counts the lines no person has accepted yet. It never changes
// the verdict - a promise nobody accepted is still a promise that broke - but
// it is reported so the owner can see a red that came from a proposal.
func (r Result) Unconfirmed() int {
	count := 0
	for _, promise := range r.Promises {
		if !promise.Confirmed {
			count++
		}
	}
	return count
}

// Summary is the one-line aggregate, naming every broken promise rather than
// stopping at the first. It is what the posted status description is built
// from, so it has to carry the whole picture in one read.
func (r Result) Summary() string {
	switch r.Kind {
	case Refused:
		return "refused: " + r.Message
	case Red:
		summary := fmt.Sprintf("%d promise(s) broken: %s", len(r.BrokenPromises()), namesOf(r.BrokenPromises()))
		if r.Incomplete {
			summary += "; also incomplete: " + r.incompleteDetail()
		}
		return summary
	default:
		if r.Incomplete {
			return "no promise broke, but the list was not fully checked: " + r.incompleteDetail()
		}
		return fmt.Sprintf("all %d promise(s) still hold", len(r.Promises))
	}
}

// StatusDescription is the compact form for the posted status, which GitHub
// caps at 140 characters. It leads with the counts and the promise ids,
// because those are what a reader acts on, and it keeps an infrastructure
// failure's headline rather than its whole message - the full reasons live in
// the printed record, which the status is not a substitute for.
func (r Result) StatusDescription() string {
	switch r.Kind {
	case Refused:
		return "refused: " + r.Message
	case Red:
		description := fmt.Sprintf("%d of %d promise(s) broken: %s", len(r.BrokenPromises()), len(r.Promises), namesOf(r.BrokenPromises()))
		if r.Incomplete {
			description += "; " + r.uncheckedHeadline()
		}
		return description
	default:
		if r.Incomplete {
			return "no promise broke, but " + r.uncheckedHeadline()
		}
		return fmt.Sprintf("all %d promise(s) still hold", len(r.Promises))
	}
}

func (r Result) uncheckedHeadline() string {
	var parts []string
	if r.Infrastructure != "" {
		parts = append(parts, firstClause(r.Infrastructure))
	}
	if unchecked := r.UncheckedPromises(); len(unchecked) > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d not checked: %s", len(unchecked), len(r.Promises), namesOf(unchecked)))
	}
	if len(parts) == 0 {
		return "an outcome was never recorded for a promise"
	}
	return strings.Join(parts, "; ")
}

// firstClause keeps the part of a failure message before its first colon -
// "the app would not start", not the whole of docker's answer - so the status
// says which kind of failure happened within the space it has.
func firstClause(message string) string {
	if cut := strings.Index(message, ":"); cut > 0 {
		return message[:cut]
	}
	return message
}

func (r Result) incompleteDetail() string {
	var parts []string
	if r.Infrastructure != "" {
		parts = append(parts, r.Infrastructure)
	}
	if unchecked := r.UncheckedPromises(); len(unchecked) > 0 {
		parts = append(parts, fmt.Sprintf("%d promise(s) not checked: %s", len(unchecked), namesOf(unchecked)))
	}
	if len(parts) == 0 {
		return "an outcome was never recorded for a promise"
	}
	return strings.Join(parts, "; ")
}

func namesOf(promises []PromiseResult) string {
	names := make([]string, 0, len(promises))
	for _, promise := range promises {
		names = append(names, promise.ID)
	}
	return strings.Join(names, ", ")
}

// ShapeCounts reports how many lines use each falsifier shape, so a run whose
// list is mostly prose cannot read as one that checked everything.
func (r Result) ShapeCounts() []string {
	counts := map[string]int{}
	for _, promise := range r.Promises {
		counts[promise.Shape]++
	}
	shapes := make([]string, 0, len(counts))
	for shape := range counts {
		shapes = append(shapes, shape)
	}
	sort.Strings(shapes)
	described := make([]string, 0, len(shapes))
	for _, shape := range shapes {
		described = append(described, fmt.Sprintf("%s: %d", shape, counts[shape]))
	}
	return described
}
