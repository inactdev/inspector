package examiner

import "testing"

func TestClassify_WeakeningChangedTestIsRedAndNamesWhatItStoppedChecking(t *testing.T) {
	tests := TestChangeList{
		BaseCommit: "abc123",
		Files: []TestChange{{
			Path: "capture_test.go", Change: "modified", Before: "assert.Equal(t, want, got)", After: "assert.NotEmpty(t, got)",
		}},
	}
	result, err := Classify(Verdict{
		Outcomes: []Outcome{{Claim: "capture saves an inkling", Verdict: Confirmed, Scenario: "POST /inklings", Evidence: "201 Created"}},
		Findings: []Finding{{
			TestPath: "capture_test.go", Detail: "the exact saved text assertion was widened to non-empty", ProposedRegression: "keep an HTTP capture test that asserts the saved text exactly matches the request",
		}},
	}, tests)
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if result.Kind != Red {
		t.Fatalf("Kind = %q, want red", result.Kind)
	}
	if result.Verdict.Findings[0].TestPath != "capture_test.go" || result.Verdict.Findings[0].Detail == "" {
		t.Fatalf("finding = %#v, want the test and what it stopped checking", result.Verdict.Findings[0])
	}
}

func TestClassify_LegitimateTestChangeDoesNotMakeTheResultRed(t *testing.T) {
	result, err := Classify(Verdict{
		Outcomes: []Outcome{{Claim: "capture saves an inkling", Verdict: Confirmed, Scenario: "POST /inklings", Evidence: "201 Created"}},
	}, TestChangeList{BaseCommit: "abc123", Files: []TestChange{{
		Path: "capture_test.go", Change: "added", After: "func TestCapture() {}",
	}}})
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if result.Kind != Green {
		t.Fatalf("Kind = %q, want green", result.Kind)
	}
}

func TestClassify_CouldNotBeTestedIsRefusedNotRed(t *testing.T) {
	result, err := Classify(Verdict{Outcomes: []Outcome{{
		Claim: "capture saves an inkling", Verdict: CouldNotBeTested, Scenario: "POST /inklings", Evidence: "the app did not start",
	}}}, TestChangeList{BaseCommit: "abc123"})
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if result.Kind != Refused {
		t.Fatalf("Kind = %q, want refused", result.Kind)
	}
	if result.Message == "" {
		t.Fatal("refusal should explain that the capability could not be tested")
	}
}

func TestClassify_RejectsFindingForUnlistedTest(t *testing.T) {
	_, err := Classify(Verdict{
		Outcomes: []Outcome{{Claim: "capture saves an inkling", Verdict: Confirmed, Scenario: "POST /inklings", Evidence: "201 Created"}},
		Findings: []Finding{{TestPath: "hidden_test.go", Detail: "deleted case", ProposedRegression: "restore it"}},
	}, TestChangeList{BaseCommit: "abc123"})
	if err == nil {
		t.Fatal("Classify() accepted a test-change finding for a test that was not supplied")
	}
}
