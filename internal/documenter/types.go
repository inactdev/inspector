// Package documenter is Inspector's documentation judgment (issue #16). It
// asks two questions about a change, not one: is the change described where it
// belongs, and - the harder one - has the change made something already
// written untrue?
//
// The second question is why this package exists. A documentation step that
// looks only at the files in the diff cannot ask it, because the document that
// went stale is the one the change never touched: a list elsewhere that no
// longer lists everything, a README promising behavior that moved, a described
// default that changed, an example command that no longer runs. So the model
// is given the project's own tracked text as a corpus it can search and read,
// and every finding it reports is validated back against that corpus.
//
// It reports and never edits, like every other inspector judgment: a
// correction is proposed text Fabrica applies through the handback loop
// (fabrica#95), and SPEC.md section 4's two limits are enforced here rather
// than asked for - a finding can only name documentation that already exists,
// and intent comes from the request or not at all.
package documenter

import (
	"errors"
	"fmt"
	"strings"
)

// FindingKind names which of the two questions a finding answers.
type FindingKind string

const (
	// FalsifiedClaim is the second question: something already written is no
	// longer true of the project because of this change. The file it is in is
	// usually not in the change at all, which is the whole reason this
	// judgment exists.
	FalsifiedClaim FindingKind = "falsified_claim"
	// UndescribedChange is the first question: the change did something that
	// belongs in a document which already covers that ground and does not
	// mention it. It can only ever name an existing document - deciding the
	// project needs one it never had is the Client's call, not inspector's.
	UndescribedChange FindingKind = "undescribed_change"
)

// Finding is one documentation problem this change caused, pointed at the
// exact file and line, carrying the claim verbatim so the pointer can be
// checked rather than taken on trust.
type Finding struct {
	Kind FindingKind `json:"kind"`
	Path string      `json:"path"`
	Line int         `json:"line"`
	// Claim is the text as the file has it. Inspector verifies it against the
	// corpus before recording the finding.
	Claim string `json:"claim"`
	// Summary says what the change did that made this claim untrue, or what
	// the document fails to describe.
	Summary string `json:"summary"`
	// Correction is the proposed replacement for Claim. Inspector never
	// writes it anywhere; Fabrica applies it after the Client's verdict.
	Correction string `json:"correction,omitempty"`
	// NeedsClientJudgment marks a claim that looks falsified but cannot be
	// corrected without a product judgment or a guess at intent. It carries
	// JudgmentReason instead of a Correction: the Client is told what the
	// claim is and why it looks falsified, and nothing is guessed on his
	// behalf.
	NeedsClientJudgment bool   `json:"needsClientJudgment,omitempty"`
	JudgmentReason      string `json:"judgmentReason,omitempty"`
	// InChange is inspector's own fact, read from the diff rather than
	// proposed by the model: was this file part of the change? A false here is
	// the thing issue #16 asks for - documentation corrected in a file the
	// change never touched.
	InChange bool `json:"inChange"`
}

// ResultKind is the overall outcome of one documentation review.
type ResultKind string

const (
	// Reviewed means the corpus was read and the review reported, including
	// when it reported nothing. Documentation that still describes reality is
	// a real result, not an incomplete one.
	Reviewed ResultKind = "reviewed"
	// Refused means the review could not be completed at all - missing
	// configuration, an unreadable change, no corpus, or a model failure.
	Refused ResultKind = "refused"
)

// Verdict is the run's single derived answer about the project's
// documentation. Inspector derives it from the record; no model submits one.
type Verdict string

const (
	// DocumentationConsistent means the review completed and found nothing:
	// the change is described where it belongs and falsified nothing.
	DocumentationConsistent Verdict = "documentation_consistent"
	// DocumentationUndescribed means the change is not described somewhere it
	// belongs, and no existing claim was found falsified.
	DocumentationUndescribed Verdict = "documentation_undescribed_change"
	// DocumentationFalsified means at least one thing already written is no
	// longer true. It outranks every other outcome: a document that lies is
	// worse than one that is merely silent, because nothing else in the
	// pipeline ever notices it.
	DocumentationFalsified Verdict = "documentation_falsified_claims"
)

// Result is the record-derived outcome of one documentation review.
type Result struct {
	Kind     ResultKind `json:"kind"`
	Verdict  Verdict    `json:"verdict,omitempty"`
	Findings []Finding  `json:"findings,omitempty"`
	Message  string     `json:"message,omitempty"`
	// Vendor names which model vendor produced this result, set by the caller
	// so the choice is never silent - the same rule the examiner and reviewer
	// follow.
	Vendor string `json:"vendor,omitempty"`
	// Corpus is what this run was able to read, and what it was not. A
	// judgment made over a partial corpus must never read like one made over
	// the whole project.
	Corpus CorpusSummary `json:"corpus"`
	// Lookups counts how many times this run searched, listed, or read
	// something in the project. It is the evidence that the second question
	// was actually asked: a review that looked at nothing outside the change
	// cannot report that nothing was falsified, and cannot finish at all.
	Lookups int `json:"corpusLookups"`
	// RequestProvided records whether inspector was given the request text
	// that says what was meant. Every completed review carries it, so a run
	// that could not judge intent never reads as one that found no intent
	// problem - the same separation the reviewer keeps for test judgment it
	// was never asked for.
	RequestProvided bool `json:"requestProvided"`
}

// NeedsClientJudgment returns every finding that could not be corrected
// without a product judgment, so they can be printed where the Client will
// actually see them rather than folded in with the rest.
func (r Result) NeedsClientJudgment() []Finding {
	var flagged []Finding
	for _, finding := range r.Findings {
		if finding.NeedsClientJudgment {
			flagged = append(flagged, finding)
		}
	}
	return flagged
}

// Record owns every finding one run reports and validates each against the
// corpus and the change before accepting it.
type Record struct {
	corpus      *Corpus
	changePaths map[string]struct{}
	Findings    []Finding
}

// NewRecord starts a record that only accepts findings naming a file in
// corpus, and marks each one with whether changePaths contains it.
func NewRecord(corpus *Corpus, changePaths map[string]struct{}) *Record {
	return &Record{corpus: corpus, changePaths: changePaths}
}

// AddFindings validates and appends a batch atomically: one invalid finding
// rejects the whole batch, so a partly-wrong report is never split into an
// accepted prefix and a rejected remainder.
func (r *Record) AddFindings(findings []Finding) error {
	if len(findings) == 0 {
		return errors.New("no findings were given")
	}
	validated := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		if err := r.validate(&finding); err != nil {
			return err
		}
		validated = append(validated, finding)
	}
	r.Findings = append(r.Findings, validated...)
	return nil
}

func (r *Record) validate(finding *Finding) error {
	switch finding.Kind {
	case FalsifiedClaim, UndescribedChange:
	default:
		return fmt.Errorf("finding kind %q is not one of %q, %q", finding.Kind, FalsifiedClaim, UndescribedChange)
	}
	if strings.TrimSpace(finding.Path) == "" {
		return errors.New("a finding has no path")
	}
	if !r.corpus.Has(finding.Path) {
		// This is SPEC.md section 4's first limit made structural rather than
		// requested: inspector reports on documentation the change made
		// untrue, and deciding the project needs a document it has never had
		// is a product judgment belonging to the Client.
		return fmt.Errorf("finding names %q, which is not a file in the project's documentation corpus; inspector reports on documentation that already exists and never proposes a document the project has never had - that is the Client's call", finding.Path)
	}
	if strings.TrimSpace(finding.Summary) == "" {
		return fmt.Errorf("finding for %s:%d does not say what the change did to this claim", finding.Path, finding.Line)
	}
	if err := r.corpus.VerifyClaim(finding.Path, finding.Line, finding.Claim); err != nil {
		return err
	}
	hasCorrection := strings.TrimSpace(finding.Correction) != ""
	switch {
	case hasCorrection && finding.NeedsClientJudgment:
		return fmt.Errorf("finding for %s:%d both proposes a correction and asks for the Client's judgment; it must do exactly one", finding.Path, finding.Line)
	case !hasCorrection && !finding.NeedsClientJudgment:
		return fmt.Errorf("finding for %s:%d neither proposes a correction nor flags that it needs the Client's judgment; one of the two is required, because the one outcome nobody wants is silence", finding.Path, finding.Line)
	case finding.NeedsClientJudgment && strings.TrimSpace(finding.JudgmentReason) == "":
		return fmt.Errorf("finding for %s:%d asks for the Client's judgment without saying why it cannot be corrected", finding.Path, finding.Line)
	}
	if !finding.NeedsClientJudgment {
		finding.JudgmentReason = ""
	}
	_, inChange := r.changePaths[finding.Path]
	finding.InChange = inChange
	return nil
}

// DeriveVerdict reduces the recorded findings to the run's one verdict.
// Safest reading first: a falsified claim outranks an undescribed change,
// which outranks consistency.
func DeriveVerdict(findings []Finding) Verdict {
	verdict := DocumentationConsistent
	for _, finding := range findings {
		switch finding.Kind {
		case FalsifiedClaim:
			return DocumentationFalsified
		case UndescribedChange:
			verdict = DocumentationUndescribed
		}
	}
	return verdict
}
