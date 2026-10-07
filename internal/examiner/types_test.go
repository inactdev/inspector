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
	if err := record.RecordAttempt("capture", AppRequest{Method: "post", Path: "/inklings"}, true, "HTTP 500"); err != nil {
		t.Fatalf("RecordAttempt() error = %v", err)
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
	if err := record.RecordAttempt("broken", AppRequest{Method: "post", Path: "/inklings"}, true, "HTTP 500"); err != nil {
		t.Fatalf("RecordAttempt() error = %v", err)
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

func TestExaminationRecord_RejectsObservationWithoutSuccessfulAttempt(t *testing.T) {
	record := NewExaminationRecord(1)
	if err := record.AddCapability(CapabilityProposal{ID: "list", Claim: "saved inklings can be listed", Scenario: "GET /inklings"}); err != nil {
		t.Fatalf("AddCapability() error = %v", err)
	}
	if err := record.RecordObservation("list", "HTTP 200"); err == nil {
		t.Fatal("RecordObservation() accepted evidence with no successful recorded attempt")
	}
}

func TestExaminationRecord_RecordAttemptNormalizesAndRetainsRequest(t *testing.T) {
	record := NewExaminationRecord(1)
	if err := record.AddCapability(CapabilityProposal{ID: "create", Claim: "an inkling can be saved", Scenario: "POST /inklings"}); err != nil {
		t.Fatalf("AddCapability() error = %v", err)
	}
	request := AppRequest{Method: "post", Path: "/inklings", Headers: map[string]string{"content-type": "application/json"}, Body: `{"text":"hello"}`}
	if err := record.RecordAttempt("create", request, true, "HTTP 201"); err != nil {
		t.Fatalf("RecordAttempt() error = %v", err)
	}
	attempt := record.Attempts["create"][0]
	if attempt.Request.Method != "POST" || attempt.Request.Path != request.Path || attempt.Request.Headers["Content-Type"] != "application/json" || attempt.Request.Body != request.Body {
		t.Fatalf("recorded request = %#v, want normalized complete request", attempt.Request)
	}
}
