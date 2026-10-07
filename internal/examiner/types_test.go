package examiner

import "testing"

func TestExaminationRecord_DerivesTestFindingFromPreTaskScenario(t *testing.T) {
	record := NewExaminationRecord(2)
	if err := record.AddCapability(CapabilityProposal{
		ID: "capture", Claim: "capture saves an inkling", Scenario: "POST /inklings", TestPath: "capture_test.go",
	}); err != nil {
		t.Fatalf("AddCapability() error = %v", err)
	}
	if err := record.StartAttempt("capture"); err != nil {
		t.Fatalf("StartAttempt() error = %v", err)
	}
	if err := record.RecordObservation("capture", "HTTP 500"); err != nil {
		t.Fatalf("RecordObservation() error = %v", err)
	}
	if err := record.AddAssessment(Assessment{
		CapabilityID: "capture", Verdict: NotConfirmed, Evidence: "HTTP 500", ProposedRegression: "keep an HTTP capture regression",
	}); err != nil {
		t.Fatalf("AddAssessment() error = %v", err)
	}
	result, err := record.DeriveResult()
	if err != nil {
		t.Fatalf("DeriveResult() error = %v", err)
	}
	if result.Kind != Red || len(result.Verdict.Findings) != 1 {
		t.Fatalf("result = %#v, want red test finding", result)
	}
	finding := result.Verdict.Findings[0]
	if finding.TestPath != "capture_test.go" || finding.Detail == "" {
		t.Fatalf("finding = %#v, want named test and lost protection", finding)
	}
}

func TestExaminationRecord_DerivesCouldNotBeTestedWithoutObservation(t *testing.T) {
	record := NewExaminationRecord(1)
	if err := record.AddCapability(CapabilityProposal{ID: "list", Claim: "saved inklings can be listed", Scenario: "GET /inklings"}); err != nil {
		t.Fatalf("AddCapability() error = %v", err)
	}
	result, err := record.DeriveResult()
	if err != nil {
		t.Fatalf("DeriveResult() error = %v", err)
	}
	if result.Kind != Refused || !result.Verdict.ExaminationIncomplete || result.Verdict.Outcomes[0].Verdict != CouldNotBeTested {
		t.Fatalf("result = %#v, want incomplete could-not-be-tested refusal", result)
	}
}

func TestExaminationRecord_PreservesBugAndIncompleteMark(t *testing.T) {
	record := NewExaminationRecord(2)
	for _, capability := range []CapabilityProposal{
		{ID: "broken", Claim: "capture saves", Scenario: "POST /inklings"},
		{ID: "unknown", Claim: "capture lists", Scenario: "GET /inklings"},
	} {
		if err := record.AddCapability(capability); err != nil {
			t.Fatalf("AddCapability() error = %v", err)
		}
	}
	if err := record.StartAttempt("broken"); err != nil {
		t.Fatalf("StartAttempt() error = %v", err)
	}
	if err := record.RecordObservation("broken", "HTTP 500"); err != nil {
		t.Fatalf("RecordObservation() error = %v", err)
	}
	if err := record.AddAssessment(Assessment{CapabilityID: "broken", Verdict: NotConfirmed, Evidence: "HTTP 500", ProposedRegression: "add regression"}); err != nil {
		t.Fatalf("AddAssessment() error = %v", err)
	}
	result, err := record.DeriveResult()
	if err != nil {
		t.Fatalf("DeriveResult() error = %v", err)
	}
	if result.Kind != Red || !result.Verdict.ExaminationIncomplete {
		t.Fatalf("result = %#v, want a bug plus incomplete examination", result)
	}
}

func TestExaminationRecord_RejectsConfirmationWithoutObservation(t *testing.T) {
	record := NewExaminationRecord(1)
	if err := record.AddCapability(CapabilityProposal{ID: "list", Claim: "saved inklings can be listed", Scenario: "GET /inklings"}); err != nil {
		t.Fatalf("AddCapability() error = %v", err)
	}
	if err := record.AddAssessment(Assessment{CapabilityID: "list", Verdict: Confirmed, Evidence: "claimed"}); err == nil {
		t.Fatal("AddAssessment() accepted confirmation without a recorded observation")
	}
}
