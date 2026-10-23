package documenter

import "testing"

func TestDeriveVerdict_AFalsifiedClaimOutranksEverything(t *testing.T) {
	// Order must not decide the verdict: a document that lies is worse than
	// one that is merely silent, whichever finding came first.
	cases := []struct {
		name     string
		findings []Finding
		want     Verdict
	}{
		{"nothing found", nil, DocumentationConsistent},
		{"only undescribed", []Finding{{Kind: UndescribedChange}}, DocumentationUndescribed},
		{"only falsified", []Finding{{Kind: FalsifiedClaim}}, DocumentationFalsified},
		{"falsified first", []Finding{{Kind: FalsifiedClaim}, {Kind: UndescribedChange}}, DocumentationFalsified},
		{"falsified last", []Finding{{Kind: UndescribedChange}, {Kind: FalsifiedClaim}}, DocumentationFalsified},
	}
	for _, test := range cases {
		if got := DeriveVerdict(test.findings); got != test.want {
			t.Errorf("%s: DeriveVerdict() = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestAddFindings_RejectsTheWholeBatchForOneBadFinding(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{"README.md": "the default is 30 seconds\n"})
	record := NewRecord(corpus, map[string]struct{}{})
	err := record.AddFindings([]Finding{
		{Kind: FalsifiedClaim, Path: "README.md", Line: 1, Claim: "the default is 30 seconds", Summary: "it is 60 now", Correction: "the default is 60 seconds"},
		{Kind: FalsifiedClaim, Path: "README.md", Line: 1, Claim: "a claim this file never made", Summary: "invented", Correction: "x"},
	})
	if err == nil {
		t.Fatal("AddFindings() accepted a batch containing an unverifiable claim")
	}
	if len(record.Findings) != 0 {
		t.Fatalf("record = %#v, want no accepted prefix left behind", record.Findings)
	}
}

func TestAddFindings_RejectsAnUnknownKind(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{"README.md": "the default is 30 seconds\n"})
	record := NewRecord(corpus, map[string]struct{}{})
	err := record.AddFindings([]Finding{{
		Kind: "needs_a_rewrite", Path: "README.md", Line: 1,
		Claim: "the default is 30 seconds", Summary: "s", Correction: "c",
	}})
	if err == nil {
		t.Fatal("AddFindings() accepted a finding answering neither question")
	}
}

func TestAddFindings_MarksWhetherTheFileWasInTheChange(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{
		"README.md": "the default is 30 seconds\n",
		"GUIDE.md":  "the default is 30 seconds\n",
	})
	record := NewRecord(corpus, map[string]struct{}{"README.md": {}})
	if err := record.AddFindings([]Finding{
		{Kind: FalsifiedClaim, Path: "README.md", Line: 1, Claim: "the default is 30 seconds", Summary: "it is 60 now", Correction: "the default is 60 seconds"},
		{Kind: FalsifiedClaim, Path: "GUIDE.md", Line: 1, Claim: "the default is 30 seconds", Summary: "it is 60 now", Correction: "the default is 60 seconds"},
	}); err != nil {
		t.Fatalf("AddFindings() error = %v", err)
	}
	if !record.Findings[0].InChange {
		t.Error("README.md is in the change but was not marked as such")
	}
	if record.Findings[1].InChange {
		t.Error("GUIDE.md is not in the change but was marked as if it were")
	}
}

func TestAddFindings_RejectsAFindingClaimingBothOutcomes(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{"README.md": "the default is 30 seconds\n"})
	record := NewRecord(corpus, map[string]struct{}{})
	err := record.AddFindings([]Finding{{
		Kind: FalsifiedClaim, Path: "README.md", Line: 1,
		Claim: "the default is 30 seconds", Summary: "it is 60 now",
		Correction: "the default is 60 seconds", NeedsClientJudgment: true, JudgmentReason: "unsure",
	}})
	if err == nil {
		t.Fatal("AddFindings() accepted a finding that both corrects and defers")
	}
}

func TestAddFindings_RejectsADeferralWithNoReason(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{"README.md": "the default is 30 seconds\n"})
	record := NewRecord(corpus, map[string]struct{}{})
	err := record.AddFindings([]Finding{{
		Kind: FalsifiedClaim, Path: "README.md", Line: 1,
		Claim: "the default is 30 seconds", Summary: "it is 60 now", NeedsClientJudgment: true,
	}})
	if err == nil {
		t.Fatal("AddFindings() accepted a deferral that never says why")
	}
}

func TestAddFindings_RejectsAnEmptyBatch(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{"README.md": "x\n"})
	if err := NewRecord(corpus, map[string]struct{}{}).AddFindings(nil); err == nil {
		t.Fatal("AddFindings() accepted a report with no findings in it")
	}
}
