package reviewer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const agentTestDiff = `diff --git a/greet.go b/greet.go
index abc1234..def5678 100644
--- a/greet.go
+++ b/greet.go
@@ -1,3 +1,3 @@
 package greet
-func Hello() string { return "hi" }
+func Hello() string { return "h" + "i" }
`

func TestRunAgent_RecordsAReportedFinding(t *testing.T) {
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
		}
		switch modelCalls {
		case 1:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"report-1","name":"report_findings","input":{"findings":[{"path":"greet.go","line":2,"summary":"string concatenation is needlessly indirect"}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"finish-1","name":"finish_review","input":{}}]}`))
		default:
			t.Errorf("unexpected model call %d", modelCalls)
		}
	}))
	defer model.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	result, err := RunAgent(context.Background(), AgentOptions{
		Diff: []byte(agentTestDiff), Model: "test-model", APIBaseURL: model.URL,
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.Kind != Reviewed || len(result.Findings) != 1 {
		t.Fatalf("result = %#v, want one recorded finding", result)
	}
	if result.Findings[0].Path != "greet.go" || result.Findings[0].Line != 2 {
		t.Fatalf("finding = %#v, want it to point at greet.go:2", result.Findings[0])
	}
}

func TestRunAgent_CleanReviewHasNoFindings(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"finish-1","name":"finish_review","input":{}}]}`))
	}))
	defer model.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	result, err := RunAgent(context.Background(), AgentOptions{
		Diff: []byte(agentTestDiff), Model: "test-model", APIBaseURL: model.URL,
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.Kind != Reviewed || len(result.Findings) != 0 {
		t.Fatalf("result = %#v, want a clean review with no findings", result)
	}
}

func TestRunAgent_RejectsFindingOutsideTheReviewedChange(t *testing.T) {
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		switch modelCalls {
		case 1:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"report-1","name":"report_findings","input":{"findings":[{"path":"nonexistent.go","line":1,"summary":"not actually in the diff"}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"finish-1","name":"finish_review","input":{}}]}`))
		default:
			t.Errorf("unexpected model call %d", modelCalls)
		}
	}))
	defer model.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	result, err := RunAgent(context.Background(), AgentOptions{
		Diff: []byte(agentTestDiff), Model: "test-model", APIBaseURL: model.URL,
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("result = %#v, want the out-of-change finding rejected rather than recorded", result)
	}
}

func TestRunAgent_RejectsFindingBatchAtomically(t *testing.T) {
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		switch modelCalls {
		case 1:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"report-1","name":"report_findings","input":{"findings":[{"path":"greet.go","line":2,"summary":"valid finding"},{"path":"greet.go","line":0,"summary":"invalid: no line"}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"finish-1","name":"finish_review","input":{}}]}`))
		default:
			t.Errorf("unexpected model call %d", modelCalls)
		}
	}))
	defer model.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	result, err := RunAgent(context.Background(), AgentOptions{
		Diff: []byte(agentTestDiff), Model: "test-model", APIBaseURL: model.URL,
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("result = %#v, want the whole batch rejected because one finding was invalid", result)
	}
}

// agentTestTestDiff changes one test file and one source file, so the model
// is offered the test-change duty for exactly one path.
const agentTestTestDiff = `diff --git a/greet.go b/greet.go
index abc1234..def5678 100644
--- a/greet.go
+++ b/greet.go
@@ -1,2 +1,2 @@
 package greet
-func Hello() string { return "hi" }
+func Hello() string { return "HI" }
diff --git a/greet_test.go b/greet_test.go
index abc1234..def5678 100644
--- a/greet_test.go
+++ b/greet_test.go
@@ -1,3 +1,2 @@
 package greet
-func TestHello(t *testing.T) { assertEqual(Hello(), "hi") }
+func TestHello(t *testing.T) { _ = Hello() }
`

func runAgentWithTests(t *testing.T, responses ...string) (Result, error) {
	t.Helper()
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		if modelCalls > len(responses) {
			t.Errorf("unexpected model call %d", modelCalls)
			return
		}
		_, _ = w.Write([]byte(responses[modelCalls-1]))
	}))
	defer model.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	return RunAgent(context.Background(), AgentOptions{
		Diff: []byte(agentTestTestDiff), TestPathPatterns: []string{"*_test.go"},
		Model: "test-model", APIBaseURL: model.URL,
	})
}

func judgeCall(id, path, judgment, reason string) string {
	return fmt.Sprintf(`{"content":[{"type":"tool_use","id":%q,"name":"judge_test_change","input":{"path":%q,"judgment":%q,"reason":%q}}]}`, id, path, judgment, reason)
}

const finishCall = `{"content":[{"type":"tool_use","id":"finish-1","name":"finish_review","input":{}}]}`

func TestRunAgent_RecordsAWeakenedTestChange(t *testing.T) {
	result, err := runAgentWithTests(t,
		judgeCall("judge-1", "greet_test.go", string(ExamWeakened), "the only assertion on Hello's output was dropped"),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.TestChangeVerdict != TestChangesWeaken {
		t.Fatalf("verdict = %q, want %q", result.TestChangeVerdict, TestChangesWeaken)
	}
	if len(result.TestChanges) != 1 || result.TestChanges[0].Path != "greet_test.go" {
		t.Fatalf("test changes = %#v, want only greet_test.go", result.TestChanges)
	}
	if result.TestChanges[0].Judgment != ExamWeakened || result.TestChanges[0].Reason == "" {
		t.Fatalf("test change = %#v, want the judgment and reason preserved", result.TestChanges[0])
	}
}

func TestRunAgent_RecordsAnIntactTestChange(t *testing.T) {
	result, err := runAgentWithTests(t,
		judgeCall("judge-1", "greet_test.go", string(ExamIntact), "same claim, asserted differently"),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.TestChangeVerdict != TestChangesIntact {
		t.Fatalf("verdict = %q, want %q", result.TestChangeVerdict, TestChangesIntact)
	}
}

func TestRunAgent_CannotTellIsRecordedAsUnjudged(t *testing.T) {
	result, err := runAgentWithTests(t,
		judgeCall("judge-1", "greet_test.go", string(ExamUnclear), "the helper it called is not in this diff"),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.TestChangeVerdict != TestChangesUnjudged {
		t.Fatalf("verdict = %q, want %q - cannot_tell must never read as intact", result.TestChangeVerdict, TestChangesUnjudged)
	}
}

func TestRunAgent_RefusesToFinishWithATestChangeStillUnjudged(t *testing.T) {
	// The model tries to finish immediately; inspector refuses, names the
	// path, and only the judged second attempt completes the review.
	result, err := runAgentWithTests(t,
		finishCall,
		judgeCall("judge-1", "greet_test.go", string(ExamWeakened), "assertion dropped"),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.Kind != Reviewed || result.TestChangeVerdict != TestChangesWeaken {
		t.Fatalf("result = %#v, want a completed review with a weakened verdict", result)
	}
}

func TestRunAgent_JudgmentAlongsideFinishInTheSameTurnCounts(t *testing.T) {
	// finish_review comes first in the block list, so answering it before
	// the judgment beside it would cost the run a whole extra turn.
	result, err := runAgentWithTests(t,
		`{"content":[
			{"type":"tool_use","id":"finish-1","name":"finish_review","input":{}},
			{"type":"tool_use","id":"judge-1","name":"judge_test_change","input":{"path":"greet_test.go","judgment":"weakens_the_exam","reason":"assertion dropped"}}
		]}`,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.Kind != Reviewed || result.TestChangeVerdict != TestChangesWeaken {
		t.Fatalf("result = %#v, want the review finished on the same turn as the judgment", result)
	}
}

func TestRunAgent_NeverFinishesWhileATestChangeIsUnjudged(t *testing.T) {
	responses := make([]string, maxAgentTurns)
	for i := range responses {
		responses[i] = finishCall
	}
	_, err := runAgentWithTests(t, responses...)
	if err == nil || !strings.Contains(err.Error(), "greet_test.go") {
		t.Fatalf("RunAgent() error = %v, want a refusal naming the test change it never judged", err)
	}
}

func TestRunAgent_RejectsAJudgmentNamingANonTestFile(t *testing.T) {
	result, err := runAgentWithTests(t,
		judgeCall("judge-1", "greet.go", string(ExamIntact), "not a test file at all"),
		judgeCall("judge-2", "greet_test.go", string(ExamIntact), "same claim, asserted differently"),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(result.TestChanges) != 1 || result.TestChanges[0].Path != "greet_test.go" {
		t.Fatalf("test changes = %#v, want the non-test judgment rejected rather than recorded", result.TestChanges)
	}
}

func TestRunAgent_WithoutTestPathsReportsThatNothingWasJudged(t *testing.T) {
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		_, _ = w.Write([]byte(finishCall))
	}))
	defer model.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	result, err := RunAgent(context.Background(), AgentOptions{
		Diff: []byte(agentTestTestDiff), Model: "test-model", APIBaseURL: model.URL,
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	// The diff plainly contains a test file, but nobody told inspector that
	// - and "not asked" must not be spelled the same as "nothing found".
	if result.TestChangeVerdict != TestJudgmentNotRequested {
		t.Fatalf("verdict = %q, want %q", result.TestChangeVerdict, TestJudgmentNotRequested)
	}
	if len(result.TestChanges) != 0 {
		t.Fatalf("test changes = %#v, want none when no pattern was given", result.TestChanges)
	}
}

func TestRunAgent_RefusesAnUnusableTestPathPattern(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	_, err := RunAgent(context.Background(), AgentOptions{
		Diff: []byte(agentTestTestDiff), TestPathPatterns: []string{"*_[tT]est.go"}, Model: "test-model",
	})
	if err == nil || !strings.Contains(err.Error(), "bracket expression") {
		t.Fatalf("RunAgent() error = %v, want the unusable pattern refused before any model call", err)
	}
}

func TestRunAgent_RefusesWithoutAModel(t *testing.T) {
	if _, err := RunAgent(context.Background(), AgentOptions{Diff: []byte(agentTestDiff)}); err == nil {
		t.Fatal("RunAgent() succeeded with no model configured")
	}
}

func TestRunAgent_RefusesOnEmptyDiff(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	_, err := RunAgent(context.Background(), AgentOptions{Diff: []byte(""), Model: "test-model"})
	if err == nil || !strings.Contains(err.Error(), "no files to review") {
		t.Fatalf("RunAgent() error = %v, want a refusal naming the empty change", err)
	}
}

func TestRunAgent_OpenAICompatibleVendorRecordsAFinding(t *testing.T) {
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		if r.URL.Path != "/chat/completions" {
			t.Errorf("model request path = %q, want /chat/completions", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch modelCalls {
		case 1:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"report-1","type":"function","function":{"name":"report_findings","arguments":"{\"findings\":[{\"path\":\"greet.go\",\"line\":2,\"summary\":\"needless concatenation\"}]}"}}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"finish-1","type":"function","function":{"name":"finish_review","arguments":"{}"}}]}}]}`))
		default:
			t.Errorf("unexpected model call %d", modelCalls)
		}
	}))
	defer model.Close()

	t.Setenv("FAKEVENDOR_API_KEY", "test-key")
	t.Setenv("FAKEVENDOR_BASE_URL", model.URL)
	result, err := RunAgent(context.Background(), AgentOptions{
		Diff: []byte(agentTestDiff), Vendor: "fakevendor", Model: "test-model",
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("result = %#v, want one finding via the OpenAI-compatible vendor", result)
	}
}
