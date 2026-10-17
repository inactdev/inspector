package reviewer

import (
	"strings"
	"testing"
)

const testChangeDiff = `diff --git a/greet.go b/greet.go
index abc1234..def5678 100644
--- a/greet.go
+++ b/greet.go
@@ -1,2 +1,2 @@
 package greet
-func Hello() string { return "hi" }
+func Hello() string { return "HI" }
diff --git a/internal/deep/greet_test.go b/internal/deep/greet_test.go
index abc1234..def5678 100644
--- a/internal/deep/greet_test.go
+++ b/internal/deep/greet_test.go
@@ -1,6 +1,4 @@
 package deep
 func TestHello(t *testing.T) {
-	if Hello() != "hi" {
-		t.Fatal("wrong greeting")
-	}
+	_ = Hello()
 }
diff --git a/gone_test.go b/gone_test.go
deleted file mode 100644
index abc1234..0000000
--- a/gone_test.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package main
-
`

func parseTestChangeDiff(t *testing.T) []File {
	t.Helper()
	files, err := ParseUnifiedDiff([]byte(testChangeDiff))
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	return files
}

func TestChangedTests_SelectsOnlyMatchingFilesWithDiffDerivedFacts(t *testing.T) {
	changes, err := ChangedTests(parseTestChangeDiff(t), []string{"*_test.go"})
	if err != nil {
		t.Fatalf("ChangedTests() error = %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %#v, want the two test files and not greet.go", changes)
	}
	if changes[0].Path != "internal/deep/greet_test.go" || changes[0].Status != TestFileModified {
		t.Fatalf("changes[0] = %#v, want the nested test file recorded as modified", changes[0])
	}
	if changes[0].LinesRemoved != 3 || changes[0].LinesAdded != 1 {
		t.Fatalf("changes[0] = %#v, want three removed lines and one added", changes[0])
	}
	if changes[1].Path != "gone_test.go" || changes[1].Status != TestFileDeleted {
		t.Fatalf("changes[1] = %#v, want the deleted test file recorded as deleted", changes[1])
	}
}

func TestChangedTests_StarCrossesDirectoryBoundariesLikeTheGate(t *testing.T) {
	// inspector-gate matches protected paths with bash's `[[ == ]]`, where
	// `*` matches `/` too. path.Match would stop at the first separator and
	// quietly miss every nested test.
	changes, err := ChangedTests(parseTestChangeDiff(t), []string{"internal/*_test.go"})
	if err != nil {
		t.Fatalf("ChangedTests() error = %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "internal/deep/greet_test.go" {
		t.Fatalf("changes = %#v, want the nested test matched through the directory boundary", changes)
	}
}

func TestChangedTests_NoPatternsSelectsNothing(t *testing.T) {
	changes, err := ChangedTests(parseTestChangeDiff(t), nil)
	if err != nil {
		t.Fatalf("ChangedTests() error = %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %#v, want nothing selected when no pattern was given", changes)
	}
}

func TestChangedTests_RefusesAPatternItCannotMatchLikeTheGate(t *testing.T) {
	_, err := ChangedTests(parseTestChangeDiff(t), []string{"*_[tT]est.go"})
	if err == nil || !strings.Contains(err.Error(), "bracket expression") {
		t.Fatalf("ChangedTests() error = %v, want a refusal naming the bracket expression", err)
	}
}

func TestChangedTests_RefusesAnEmptyPattern(t *testing.T) {
	if _, err := ChangedTests(parseTestChangeDiff(t), []string{"  "}); err == nil {
		t.Fatal("ChangedTests() accepted an empty pattern, which would match nothing silently")
	}
}

func TestDeriveTestChangeVerdict(t *testing.T) {
	weakened := TestChange{Path: "a_test.go", Judgment: ExamWeakened, Reason: "assertion dropped"}
	intact := TestChange{Path: "b_test.go", Judgment: ExamIntact, Reason: "same claim, new wording"}
	unclear := TestChange{Path: "c_test.go", Judgment: ExamUnclear, Reason: "not readable from the diff"}
	unjudged := TestChange{Path: "d_test.go"}

	cases := []struct {
		name      string
		requested bool
		changes   []TestChange
		want      TestChangeVerdict
	}{
		{"no patterns given", false, nil, TestJudgmentNotRequested},
		{"no patterns given but tests changed", false, []TestChange{intact}, TestJudgmentNotRequested},
		{"patterns given, nothing matched", true, nil, NoTestChanges},
		{"every change intact", true, []TestChange{intact, intact}, TestChangesIntact},
		{"one weakened outranks intact", true, []TestChange{intact, weakened}, TestChangesWeaken},
		{"one weakened outranks unjudged", true, []TestChange{unjudged, weakened}, TestChangesWeaken},
		{"cannot_tell never reads as intact", true, []TestChange{intact, unclear}, TestChangesUnjudged},
		{"unjudged never reads as intact", true, []TestChange{intact, unjudged}, TestChangesUnjudged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveTestChangeVerdict(tc.requested, tc.changes); got != tc.want {
				t.Fatalf("DeriveTestChangeVerdict() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecord_JudgeTestChangeRecordsAJudgment(t *testing.T) {
	record := NewRecord(nil, []TestChange{{Path: "a_test.go", Status: TestFileModified}})
	if err := record.JudgeTestChange("a_test.go", ExamWeakened, "the only assertion was removed"); err != nil {
		t.Fatalf("JudgeTestChange() error = %v", err)
	}
	if record.TestChanges[0].Judgment != ExamWeakened || record.TestChanges[0].Reason == "" {
		t.Fatalf("change = %#v, want the judgment and its reason recorded", record.TestChanges[0])
	}
	if paths := record.UnjudgedTestPaths(); len(paths) != 0 {
		t.Fatalf("UnjudgedTestPaths() = %v, want none left", paths)
	}
}

func TestRecord_JudgeTestChangeRejectsAPathThatIsNotAChangedTest(t *testing.T) {
	record := NewRecord(map[string]struct{}{"greet.go": {}}, []TestChange{{Path: "a_test.go"}})
	err := record.JudgeTestChange("greet.go", ExamIntact, "a file in the diff, but not a test")
	if err == nil || !strings.Contains(err.Error(), "not a changed test file") {
		t.Fatalf("JudgeTestChange() error = %v, want it rejected by name", err)
	}
}

func TestRecord_JudgeTestChangeRejectsASecondJudgmentForTheSamePath(t *testing.T) {
	// Otherwise a weakened verdict could be written over with an intact one.
	record := NewRecord(nil, []TestChange{{Path: "a_test.go"}})
	if err := record.JudgeTestChange("a_test.go", ExamWeakened, "assertion dropped"); err != nil {
		t.Fatalf("first JudgeTestChange() error = %v", err)
	}
	err := record.JudgeTestChange("a_test.go", ExamIntact, "on reflection, fine")
	if err == nil || !strings.Contains(err.Error(), "already been judged") {
		t.Fatalf("second JudgeTestChange() error = %v, want it refused", err)
	}
	if record.TestChanges[0].Judgment != ExamWeakened {
		t.Fatalf("judgment = %q, want the first one kept", record.TestChanges[0].Judgment)
	}
}

func TestRecord_JudgeTestChangeRejectsAnUnknownVerdictOrMissingReason(t *testing.T) {
	record := NewRecord(nil, []TestChange{{Path: "a_test.go"}})
	if err := record.JudgeTestChange("a_test.go", TestJudgment("probably_fine"), "a reason"); err == nil {
		t.Fatal("JudgeTestChange() accepted a verdict outside the three known ones")
	}
	if err := record.JudgeTestChange("a_test.go", ExamWeakened, "  "); err == nil {
		t.Fatal("JudgeTestChange() accepted a judgment with no reason")
	}
	if record.TestChanges[0].Judged() {
		t.Fatalf("change = %#v, want it still unjudged after both rejections", record.TestChanges[0])
	}
}

func TestRenderTestChanges_NamesEachPathAndItsDiffFacts(t *testing.T) {
	rendered := RenderTestChanges([]TestChange{
		{Path: "a_test.go", Status: TestFileDeleted, LinesRemoved: 12},
	})
	if !strings.Contains(rendered, "CHANGED TESTS") || !strings.Contains(rendered, "a_test.go  deleted  +0/-12") {
		t.Fatalf("rendered = %q, want the path with its derived facts", rendered)
	}
}
