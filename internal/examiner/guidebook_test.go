package examiner

import (
	"strings"
	"testing"
)

func generated(confirmedLine string) []byte {
	return []byte("---\nproject: orchard\ncommit: abc123\n" + confirmedLine + "\n---\n\n# guidebook\n\nhow to drive it.\n")
}

func TestParseGuidebookConfirmation_AcceptsADateAndAName(t *testing.T) {
	state := ParseGuidebookConfirmation(generated("confirmed: 2026-10-25 Jane Mallory"))
	if !state.Present || !state.Confirmed {
		t.Fatalf("state = %#v, want a confirmed guidebook", state)
	}
	if state.Date != "2026-10-25" || state.Name != "Jane Mallory" {
		t.Errorf("state = %#v, want the date and name read back", state)
	}
	if state.Unconfirmed() {
		t.Error("Unconfirmed() = true for a signed guidebook")
	}
	if !strings.Contains(state.Describe(), "confirmed on 2026-10-25 by Jane Mallory") {
		t.Errorf("Describe() = %q", state.Describe())
	}
}

// TestParseGuidebookConfirmation_RefusesEverythingShortOfASignature covers each
// way a line can look like a confirmation without being one. All of them have
// to fail: this single line is what decides whether an examination may confirm
// anything at all.
func TestParseGuidebookConfirmation_RefusesEverythingShortOfASignature(t *testing.T) {
	for _, line := range []string{
		"confirmed: " + GuidebookUnconfirmedValue,
		"confirmed: no",
		"confirmed: yes",
		"confirmed:",
		"confirmed:    ",
		"confirmed: Jane Mallory",
		"confirmed: 2026-10-25",
		"confirmed: 2026-13-45 Jane Mallory",
		"confirmed: 25/10/2026 Jane Mallory",
		"confirmed: today Jane Mallory",
		"confirmed: 2026-10-25 .",
		"confirmed: 2026-10-25 Your Name",
		"confirmed: 2026-10-25 your name",
	} {
		state := ParseGuidebookConfirmation(generated(line))
		if !state.Present {
			t.Errorf("%q: Present = false, want a guidebook whose line governs the run", line)
		}
		if state.Confirmed {
			t.Errorf("%q was accepted as a confirmation", line)
		}
		if state.Reason == "" {
			t.Errorf("%q: Reason is empty, want it said why", line)
		}
		if !strings.Contains(state.Describe(), "UNCONFIRMED") {
			t.Errorf("%q: Describe() = %q, want it unmistakable", line, state.Describe())
		}
	}
}

func TestParseGuidebookConfirmation_AcceptsAQuotedSignature(t *testing.T) {
	state := ParseGuidebookConfirmation(generated(`confirmed: "2026-10-25 Jane Mallory"`))
	if !state.Confirmed || state.Name != "Jane Mallory" {
		t.Fatalf("state = %#v, want a quoted signature read the same way", state)
	}
}

// TestParseGuidebookConfirmation_RefusesTwoSignatures - two means nobody can
// say which one the run was judged under.
func TestParseGuidebookConfirmation_RefusesTwoSignatures(t *testing.T) {
	document := []byte("---\nconfirmed: 2026-10-25 Jane Mallory\nconfirmed: 2020-01-01 Someone Else\n---\n")
	state := ParseGuidebookConfirmation(document)
	if state.Confirmed || !state.Present {
		t.Fatalf("state = %#v, want two signatures refused", state)
	}
	if !strings.Contains(state.Reason, "which signature governs") {
		t.Errorf("Reason = %q", state.Reason)
	}
}

// TestParseGuidebookConfirmation_TreatsAHandWrittenFeatureMapAsItsOwn is the
// behavior every caller had before guidebooks existed: a feature map somebody
// wrote and committed is not an unconfirmed draft, and the result says which of
// the two it was rather than staying silent.
func TestParseGuidebookConfirmation_TreatsAHandWrittenFeatureMapAsItsOwn(t *testing.T) {
	state := ParseGuidebookConfirmation([]byte("# Inkwell examiner feature map\n\nStart the app with ./dev.sh up -d.\n"))
	if state.Present || state.Confirmed || state.Unconfirmed() {
		t.Fatalf("state = %#v, want a hand-written map left to behave as before", state)
	}
	if !strings.Contains(state.Describe(), "no guidebook confirmation line") {
		t.Errorf("Describe() = %q, want the absence named", state.Describe())
	}
}

// TestParseGuidebookConfirmation_RefusesFrontMatterWithNoConfirmedLine fails in
// the safe direction: a document laid out like a generated one, with the line
// that would have signed it deleted, is not a document anybody vouched for.
func TestParseGuidebookConfirmation_RefusesFrontMatterWithNoConfirmedLine(t *testing.T) {
	state := ParseGuidebookConfirmation([]byte("---\nproject: orchard\n---\n\n# guidebook\n"))
	if !state.Present || state.Confirmed {
		t.Fatalf("state = %#v, want front matter with no signature treated as unsigned", state)
	}
}

// TestParseGuidebookConfirmation_FindsTheLineInBrokenFrontMatter is the other
// half of failing safe: a generated guidebook that lost its closing marker must
// not be mistaken for a hand-written document.
func TestParseGuidebookConfirmation_FindsTheLineInBrokenFrontMatter(t *testing.T) {
	state := ParseGuidebookConfirmation([]byte("---\nproject: orchard\nconfirmed: no\n\n# guidebook\n"))
	if !state.Present || state.Confirmed {
		t.Fatalf("state = %#v, want the unsigned line found even with the block unclosed", state)
	}
}

// TestParseGuidebookConfirmation_IgnoresASignatureBuriedInTheBody keeps a long
// feature map that happens to discuss confirmation from being read as signed.
func TestParseGuidebookConfirmation_IgnoresASignatureBuriedInTheBody(t *testing.T) {
	var document strings.Builder
	document.WriteString("# feature map\n")
	for n := 0; n < guidebookHeadLines+5; n++ {
		document.WriteString("prose\n")
	}
	document.WriteString("confirmed: 2026-10-25 Jane Mallory\n")
	state := ParseGuidebookConfirmation([]byte(document.String()))
	if state.Confirmed {
		t.Fatal("a line far below the front matter was accepted as a signature")
	}
}

// TestDeriveResult_AnUnconfirmedGuidebookCanNeverProduceAPass is the ruling:
// the examiner treats an unconfirmed guidebook as could-not-test, never as a
// pass. The response arrived; what it proves is that the draft's own scenario
// ran, which is not the same as the app doing what was asked.
func TestDeriveResult_AnUnconfirmedGuidebookCanNeverProduceAPass(t *testing.T) {
	record := NewExaminationRecord(4)
	record.Guidebook = ParseGuidebookConfirmation(generated("confirmed: no"))
	confirmCapability(t, record, "cap-1", Confirmed, "")

	result, err := record.DeriveResult()
	if err != nil {
		t.Fatalf("DeriveResult() error = %v", err)
	}
	if got := result.Verdict.Outcomes[0].Verdict; got != CouldNotBeTested {
		t.Fatalf("outcome verdict = %q, want %q", got, CouldNotBeTested)
	}
	if !result.Verdict.ExaminationIncomplete {
		t.Error("ExaminationIncomplete = false, want the run marked incomplete")
	}
	if result.Kind != Refused {
		t.Errorf("Kind = %q, want %q so the examiner status is error rather than success", result.Kind, Refused)
	}
	if !result.GuidebookWithheldPass || !strings.Contains(result.Guidebook, "UNCONFIRMED") {
		t.Errorf("result = %#v, want the unconfirmed guidebook carried in the result", result)
	}
	if !strings.Contains(result.Verdict.Outcomes[0].Evidence, "what was observed: a 201 was returned") {
		// The observation is kept rather than thrown away: it is still the
		// record of what happened, just not a pass.
		t.Errorf("evidence = %q, want the observation preserved alongside the reason", result.Verdict.Outcomes[0].Evidence)
	}
	if !strings.Contains(result.Message, "confirm the guidebook") {
		t.Errorf("Message = %q, want it to name the fix", result.Message)
	}
}

// TestDeriveResult_AnUnconfirmedGuidebookStillReportsABug - unreviewed
// instructions are a reason to doubt a pass, never a reason to clear a failure.
func TestDeriveResult_AnUnconfirmedGuidebookStillReportsABug(t *testing.T) {
	record := NewExaminationRecord(4)
	record.Guidebook = ParseGuidebookConfirmation(generated("confirmed: no"))
	confirmCapability(t, record, "cap-1", NotConfirmed, "assert the basket is kept")

	result, err := record.DeriveResult()
	if err != nil {
		t.Fatalf("DeriveResult() error = %v", err)
	}
	if result.Kind != Red || result.Verdict.Outcomes[0].Verdict != NotConfirmed {
		t.Fatalf("result = %#v, want a found bug left red", result)
	}
}

// TestDeriveResult_AnUnreachableAppIsNotBlamedOnTheGuidebook keeps the reason
// honest. An unsigned guidebook is only the explanation when it actually
// withheld a pass; here nothing was confirmable in the first place, and
// sending somebody to sign a guidebook would be sending them to the wrong fix.
func TestDeriveResult_AnUnreachableAppIsNotBlamedOnTheGuidebook(t *testing.T) {
	record := NewExaminationRecord(4)
	record.Guidebook = ParseGuidebookConfirmation(generated("confirmed: no"))
	if err := record.AddCapability(CapabilityProposal{ID: "cap-1", Claim: "a basket is kept", Scenario: "create one"}); err != nil {
		t.Fatalf("AddCapability() error = %v", err)
	}
	if err := record.StartAttempt("cap-1", AppRequest{Method: "POST", Path: "/baskets"}); err != nil {
		t.Fatalf("StartAttempt() error = %v", err)
	}
	if err := record.CompleteAttempt("cap-1", false, "driver error: calling app: connection refused"); err != nil {
		t.Fatalf("CompleteAttempt() error = %v", err)
	}

	result, err := record.DeriveResult()
	if err != nil {
		t.Fatalf("DeriveResult() error = %v", err)
	}
	if result.Verdict.Outcomes[0].Verdict != CouldNotBeTested || result.Kind != Refused {
		t.Fatalf("result = %#v, want could-not-be-tested and no verdict", result)
	}
	if result.GuidebookWithheldPass {
		t.Error("GuidebookWithheldPass = true, but there was no pass for the guidebook to withhold")
	}
	if strings.Contains(result.Message, "confirm the guidebook") {
		t.Errorf("Message = %q, want the unreachable app named rather than the guidebook", result.Message)
	}
	if !strings.Contains(result.Guidebook, "UNCONFIRMED") {
		// Still reported: the reader must know the guidebook was unsigned too.
		t.Errorf("Guidebook = %q, want the unsigned guidebook still stated", result.Guidebook)
	}
}

// TestDeriveResult_AConfirmedGuidebookChangesNothing keeps the gate narrow.
func TestDeriveResult_AConfirmedGuidebookChangesNothing(t *testing.T) {
	for _, guidebook := range []GuidebookConfirmation{
		ParseGuidebookConfirmation(generated("confirmed: 2026-10-25 Jane Mallory")),
		ParseGuidebookConfirmation([]byte("# a hand-written feature map\n")),
		{},
	} {
		record := NewExaminationRecord(4)
		record.Guidebook = guidebook
		confirmCapability(t, record, "cap-1", Confirmed, "")

		result, err := record.DeriveResult()
		if err != nil {
			t.Fatalf("DeriveResult() error = %v", err)
		}
		if result.Kind != Green || result.Verdict.Outcomes[0].Verdict != Confirmed {
			t.Fatalf("result = %#v for guidebook %#v, want an ordinary pass", result, guidebook)
		}
		if result.Verdict.ExaminationIncomplete {
			t.Error("ExaminationIncomplete = true on a complete examination")
		}
	}
}

// confirmCapability drives one capability through the record the way the agent
// does: propose, attempt, deliver the response, then assess.
func confirmCapability(t *testing.T, record *ExaminationRecord, id string, verdict OutcomeVerdict, regression string) {
	t.Helper()
	evidence := "a 201 was returned"
	if err := record.AddCapability(CapabilityProposal{ID: id, Claim: "a basket is kept", Scenario: "create one"}); err != nil {
		t.Fatalf("AddCapability() error = %v", err)
	}
	if err := record.StartAttempt(id, AppRequest{Method: "POST", Path: "/baskets"}); err != nil {
		t.Fatalf("StartAttempt() error = %v", err)
	}
	if err := record.CompleteAttempt(id, true, evidence); err != nil {
		t.Fatalf("CompleteAttempt() error = %v", err)
	}
	if err := record.RecordObservation(id, evidence); err != nil {
		t.Fatalf("RecordObservation() error = %v", err)
	}
	if err := record.AddAssessment(Assessment{CapabilityID: id, Verdict: verdict, Evidence: evidence, ProposedRegression: regression}); err != nil {
		t.Fatalf("AddAssessment() error = %v", err)
	}
}
