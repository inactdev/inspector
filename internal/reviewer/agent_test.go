package reviewer

import (
	"context"
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
