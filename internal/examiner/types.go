// Package examiner independently judges requested observable behavior against a
// running HTTP app without mounting the application's source code.
package examiner

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const (
	InputRequestName      = "request.md"
	InputFeatureMapName   = "feature-map.md"
	InputAlwaysTrueName   = "always-true.md"
	InputChangedFilesName = "changed-files.json"
	InputBaseTestsName    = "base-tests.json"
	VerdictName           = "verdict.json"
)

// OutcomeVerdict is the examiner's judgment of one capability it derived from
// the request, feature map, always-true list, or a pre-task test.
type OutcomeVerdict string

const (
	Confirmed        OutcomeVerdict = "confirmed"
	NotConfirmed     OutcomeVerdict = "not_confirmed"
	CouldNotBeTested OutcomeVerdict = "could_not_be_tested"
)

// Outcome records a capability and the scenario used to operate the app.
type Outcome struct {
	ID                 string         `json:"id"`
	Claim              string         `json:"claim"`
	Verdict            OutcomeVerdict `json:"verdict"`
	Scenario           string         `json:"scenario"`
	Evidence           string         `json:"evidence"`
	TestPath           string         `json:"testPath,omitempty"`
	Attempts           []AppAttempt   `json:"attempts"`
	ProposedRegression string         `json:"proposedRegression,omitempty"`
}

// AppRequest is the HTTP request used for an app-driving attempt.
type AppRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// AppAttempt is one machine-recorded attempt to operate a capability.
type AppAttempt struct {
	Request    AppRequest `json:"request"`
	Evidence   string     `json:"evidence"`
	Successful bool       `json:"successful"`
}

// Finding is a confirmed missing behavior protected by a changed test. It is a
// proposal for the project's own check suite, never an edit to that suite.
type Finding struct {
	TestPath           string `json:"testPath"`
	Detail             string `json:"detail"`
	ProposedRegression string `json:"proposedRegression"`
}

// Verdict is the record-derived, machine-readable examination result.
type Verdict struct {
	Outcomes              []Outcome `json:"outcomes"`
	Findings              []Finding `json:"findings,omitempty"`
	ExaminationIncomplete bool      `json:"examinationIncomplete"`
}

// ResultKind is the overall result after every individual outcome is retained.
type ResultKind string

const (
	Green   ResultKind = "green"
	Red     ResultKind = "red"
	Refused ResultKind = "refused"
)

// Result contains the record-derived per-outcome verdict plus its overall
// publication result. Red and Incomplete can both be true: a real bug remains
// a failure even when another capability could not be tested.
type Result struct {
	Kind    ResultKind `json:"kind"`
	Verdict Verdict    `json:"verdict"`
	Message string     `json:"message,omitempty"`
}

// ChangedFileList is Fabrica's names-only record of every worker-changed file.
// It intentionally carries no worker-written content or diff.
type ChangedFileList struct {
	BaseCommit string        `json:"baseCommit"`
	Files      []ChangedFile `json:"files"`
}

// ChangedFile names one worker-changed path. PreviousPath is a name only and
// distinguishes a rename from a new file.
type ChangedFile struct {
	Path         string `json:"path"`
	PreviousPath string `json:"previousPath,omitempty"`
	Change       string `json:"change"`
}

// BaseTestList contains only pre-worker versions of changed test files. Fabrica
// reads these from the task's starting commit; the worker's test content never
// enters the examiner.
type BaseTestList struct {
	BaseCommit string     `json:"baseCommit"`
	Tests      []BaseTest `json:"tests"`
}

// BaseTest is a test file's immutable task-starting version.
type BaseTest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// CapabilityProposal is the model's suggestion for a scenario. It is not a
// verdict. The machinery owns attempts, observations, and final outcomes.
type CapabilityProposal struct {
	ID       string `json:"id"`
	Claim    string `json:"claim"`
	Scenario string `json:"scenario"`
	TestPath string `json:"testPath,omitempty"`
}

// Observation is a successful HTTP response delivered to the model on a later
// turn. It is the record that makes an outcome eligible for confirmation.
type Observation struct {
	CapabilityID string `json:"capabilityId"`
	Evidence     string `json:"evidence"`
}

// Assessment is the model's interpretation of an already-delivered successful
// observation. The record derives its final outcome from this proposal.
type Assessment struct {
	CapabilityID       string         `json:"capabilityId"`
	Verdict            OutcomeVerdict `json:"verdict"`
	Evidence           string         `json:"evidence"`
	ProposedRegression string         `json:"proposedRegression,omitempty"`
}

// ExaminationRecord owns all app attempts and model proposals for one run.
type ExaminationRecord struct {
	Capabilities map[string]CapabilityProposal
	Observations map[string][]Observation
	Assessments  map[string]Assessment
	Attempts     map[string][]AppAttempt
	AttemptCount int
	Budget       int
}

// NewExaminationRecord starts a bounded record. The model never writes a final
// verdict; it can only add proposals that this record validates and derives.
func NewExaminationRecord(budget int) *ExaminationRecord {
	return &ExaminationRecord{
		Capabilities: make(map[string]CapabilityProposal),
		Observations: make(map[string][]Observation),
		Assessments:  make(map[string]Assessment),
		Attempts:     make(map[string][]AppAttempt),
		Budget:       budget,
	}
}

// AddCapability accepts a proposed scenario before it is driven.
func (r *ExaminationRecord) AddCapability(proposal CapabilityProposal) error {
	if strings.TrimSpace(proposal.ID) == "" {
		return errors.New("capability proposal has no id")
	}
	if _, exists := r.Capabilities[proposal.ID]; exists {
		return fmt.Errorf("capability proposal %q was already recorded", proposal.ID)
	}
	if strings.TrimSpace(proposal.Claim) == "" || strings.TrimSpace(proposal.Scenario) == "" {
		return fmt.Errorf("capability proposal %q needs a claim and scenario", proposal.ID)
	}
	r.Capabilities[proposal.ID] = proposal
	return nil
}

// StartAttempt reserves budget for a capability before its HTTP request runs.
func (r *ExaminationRecord) StartAttempt(capabilityID string) error {
	if _, exists := r.Capabilities[capabilityID]; !exists {
		return fmt.Errorf("attempt names unknown capability %q", capabilityID)
	}
	if r.Budget > 0 && r.AttemptCount >= r.Budget {
		return fmt.Errorf("examination attempt budget of %d is exhausted", r.Budget)
	}
	r.AttemptCount++
	return nil
}

// RecordAttempt retains the driver response or failure for the capability that
// initiated it. A successful attempt becomes an observation only after that
// response was delivered back to the model in a later turn.
func (r *ExaminationRecord) RecordAttempt(capabilityID string, request AppRequest, successful bool, evidence string) error {
	if _, exists := r.Capabilities[capabilityID]; !exists {
		return fmt.Errorf("attempt names unknown capability %q", capabilityID)
	}
	if strings.TrimSpace(evidence) == "" {
		return fmt.Errorf("attempt for %q has no evidence", capabilityID)
	}
	r.Attempts[capabilityID] = append(r.Attempts[capabilityID], AppAttempt{
		Request: normalizedAppRequest(request), Evidence: evidence, Successful: successful,
	})
	return nil
}

func normalizedAppRequest(request AppRequest) AppRequest {
	normalized := request
	normalized.Method = strings.ToUpper(request.Method)
	if request.Headers == nil {
		normalized.Headers = map[string]string{}
		return normalized
	}
	normalized.Headers = make(map[string]string, len(request.Headers))
	names := make([]string, 0, len(request.Headers))
	for name := range request.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		normalized.Headers[http.CanonicalHeaderKey(name)] = request.Headers[name]
	}
	return normalized
}

// RecordObservation records only a successful response that was delivered to
// the model before a later assessment can refer to it.
func (r *ExaminationRecord) RecordObservation(capabilityID, evidence string) error {
	if _, exists := r.Capabilities[capabilityID]; !exists {
		return fmt.Errorf("observation names unknown capability %q", capabilityID)
	}
	if strings.TrimSpace(evidence) == "" {
		return fmt.Errorf("observation for %q has no evidence", capabilityID)
	}
	for _, attempt := range r.Attempts[capabilityID] {
		if attempt.Successful && attempt.Evidence == evidence {
			r.Observations[capabilityID] = append(r.Observations[capabilityID], Observation{CapabilityID: capabilityID, Evidence: evidence})
			return nil
		}
	}
	return fmt.Errorf("observation for %q has no matching successful attempt", capabilityID)
}

// AddAssessment records a model proposal only after the machine recorded a
// successful observation for the same capability.
func (r *ExaminationRecord) AddAssessment(assessment Assessment) error {
	if _, exists := r.Capabilities[assessment.CapabilityID]; !exists {
		return fmt.Errorf("assessment names unknown capability %q", assessment.CapabilityID)
	}
	if len(r.Observations[assessment.CapabilityID]) == 0 {
		return fmt.Errorf("assessment for %q has no successful observation", assessment.CapabilityID)
	}
	if _, exists := r.Assessments[assessment.CapabilityID]; exists {
		return fmt.Errorf("capability %q was already assessed", assessment.CapabilityID)
	}
	if strings.TrimSpace(assessment.Evidence) == "" {
		return fmt.Errorf("assessment for %q has no evidence", assessment.CapabilityID)
	}
	switch assessment.Verdict {
	case Confirmed:
	case NotConfirmed:
		if strings.TrimSpace(assessment.ProposedRegression) == "" {
			return fmt.Errorf("assessment for %q is not confirmed but proposes no regression line", assessment.CapabilityID)
		}
	default:
		return fmt.Errorf("assessment for %q has invalid verdict %q", assessment.CapabilityID, assessment.Verdict)
	}
	r.Assessments[assessment.CapabilityID] = assessment
	return nil
}

// DeriveResult computes each outcome from the recorded evidence. A capability
// with no successful observation, or no assessment of one, is explicitly
// could-not-be-tested. A found failure remains Red even when the examination is
// incomplete; the separate incomplete mark preserves both facts.
func (r *ExaminationRecord) DeriveResult() (Result, error) {
	if len(r.Capabilities) == 0 {
		return Result{}, errors.New("examiner proposed no capabilities")
	}
	verdict := Verdict{}
	result := Result{Kind: Green}
	for _, id := range sortedCapabilityIDs(r.Capabilities) {
		capability := r.Capabilities[id]
		outcome := Outcome{ID: id, Claim: capability.Claim, Scenario: capability.Scenario, TestPath: capability.TestPath, Attempts: r.Attempts[id]}
		assessment, assessed := r.Assessments[id]
		if len(r.Observations[id]) == 0 || !assessed {
			outcome.Verdict = CouldNotBeTested
			if len(r.Observations[id]) == 0 {
				if attempts := r.Attempts[id]; len(attempts) > 0 {
					outcome.Evidence = attempts[len(attempts)-1].Evidence
				} else {
					outcome.Evidence = "no app attempt was recorded for this capability"
				}
			} else {
				outcome.Evidence = "the app response was recorded but no later assessment was proposed"
			}
			verdict.ExaminationIncomplete = true
			verdict.Outcomes = append(verdict.Outcomes, outcome)
			continue
		}
		outcome.Verdict = assessment.Verdict
		outcome.Evidence = assessment.Evidence
		outcome.ProposedRegression = assessment.ProposedRegression
		verdict.Outcomes = append(verdict.Outcomes, outcome)
		if outcome.Verdict == NotConfirmed {
			result.Kind = Red
			if outcome.TestPath != "" {
				verdict.Findings = append(verdict.Findings, Finding{
					TestPath:           outcome.TestPath,
					Detail:             "the pre-task scenario it protected is not confirmed: " + outcome.Claim,
					ProposedRegression: outcome.ProposedRegression,
				})
			}
		}
	}
	if result.Kind == Green && verdict.ExaminationIncomplete {
		result.Kind = Refused
		result.Message = "one or more claimed capabilities could not be tested; this is not a finding that the work is wrong"
	}
	result.Verdict = verdict
	return result, nil
}

func sortedCapabilityIDs(capabilities map[string]CapabilityProposal) []string {
	ids := make([]string, 0, len(capabilities))
	for id := range capabilities {
		ids = append(ids, id)
	}
	for n := 1; n < len(ids); n++ {
		for m := n; m > 0 && ids[m] < ids[m-1]; m-- {
			ids[m], ids[m-1] = ids[m-1], ids[m]
		}
	}
	return ids
}
