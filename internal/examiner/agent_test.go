package examiner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAgent_RecordDerivesAConfirmedOutcome(t *testing.T) {
	appCalls := 0
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appCalls++
		if r.Method != http.MethodGet || r.URL.Path != "/inklings" {
			t.Errorf("app request = %s %s, want GET /inklings", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"one","text":"hello"}]`))
	}))
	defer app.Close()

	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
		}
		switch modelCalls {
		case 1:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"propose-1","name":"propose_capabilities","input":{"capabilities":[{"id":"list","claim":"saved inklings can be listed","scenario":"GET /inklings"}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"drive-1","name":"drive_app","input":{"capabilityId":"list","request":{"method":"GET","path":"/inklings"}}}]}`))
		case 3:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"assess-1","name":"assess_capabilities","input":{"assessments":[{"capabilityId":"list","verdict":"confirmed","evidence":"200 response contained the saved inkling"}]}}]}`))
		case 4:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"finish-1","name":"finish_examination","input":{}}]}`))
		default:
			t.Errorf("unexpected model call %d", modelCalls)
		}
	}))
	defer model.Close()

	inputDir, outputDir := agentDirectories(t)
	t.Setenv(AnthropicAPIKeyEnvVar, "test-key")
	if err := RunAgent(context.Background(), AgentOptions{
		InputDir: inputDir, OutputDir: outputDir, AppURL: app.URL, Model: "test-model", APIBaseURL: model.URL, Budget: 4,
	}); err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if appCalls != 1 {
		t.Fatalf("app calls = %d, want 1", appCalls)
	}
	data, err := os.ReadFile(filepath.Join(outputDir, VerdictName))
	if err != nil {
		t.Fatalf("reading verdict: %v", err)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decoding verdict: %v", err)
	}
	if result.Kind != Green || result.Verdict.Outcomes[0].Verdict != Confirmed {
		t.Fatalf("verdict = %#v, want a record-derived confirmation", result)
	}
	attempts := result.Verdict.Outcomes[0].Attempts
	if len(attempts) != 1 || attempts[0].Request.Method != http.MethodGet || attempts[0].Request.Path != "/inklings" {
		t.Fatalf("attempts = %#v, want the driven request recorded with its response", attempts)
	}
}

func TestRunAgent_UnreachableAppDerivesNamedCouldNotBeTestedOutcome(t *testing.T) {
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		switch modelCalls {
		case 1:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"propose-1","name":"propose_capabilities","input":{"capabilities":[{"id":"list","claim":"saved inklings can be listed","scenario":"GET /inklings"}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"drive-1","name":"drive_app","input":{"capabilityId":"list","request":{"method":"GET","path":"/inklings"}}}]}`))
		case 3:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"finish-1","name":"finish_examination","input":{}}]}`))
		}
	}))
	defer model.Close()

	inputDir, outputDir := agentDirectories(t)
	t.Setenv(AnthropicAPIKeyEnvVar, "test-key")
	if err := RunAgent(context.Background(), AgentOptions{
		InputDir: inputDir, OutputDir: outputDir, AppURL: "http://127.0.0.1:1", Model: "test-model", APIBaseURL: model.URL, Budget: 4,
	}); err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outputDir, VerdictName))
	if err != nil {
		t.Fatalf("reading verdict: %v", err)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decoding verdict: %v", err)
	}
	if result.Kind != Refused || !result.Verdict.ExaminationIncomplete || result.Verdict.Outcomes[0].Verdict != CouldNotBeTested {
		t.Fatalf("verdict = %#v, want named could-not-be-tested refusal", result)
	}
	attempts := result.Verdict.Outcomes[0].Attempts
	if len(attempts) != 1 || attempts[0].Successful {
		t.Fatalf("attempts = %#v, want one recorded failed app attempt", attempts)
	}
}

func TestRunAgent_RejectsAssessmentBeforeDeliveredObservation(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"propose-1","name":"propose_capabilities","input":{"capabilities":[{"id":"list","claim":"saved inklings can be listed","scenario":"GET /inklings"}]}},{"type":"tool_use","id":"assess-1","name":"assess_capabilities","input":{"assessments":[{"capabilityId":"list","verdict":"confirmed","evidence":"claimed"}]}}]}`))
	}))
	defer model.Close()

	inputDir, outputDir := agentDirectories(t)
	t.Setenv(AnthropicAPIKeyEnvVar, "test-key")
	err := RunAgent(context.Background(), AgentOptions{
		InputDir: inputDir, OutputDir: outputDir, AppURL: "http://127.0.0.1:8080", Model: "test-model", APIBaseURL: model.URL,
	})
	if err == nil {
		t.Fatal("RunAgent() accepted an assessment without a delivered observation")
	}
}

func TestRunAgent_RejectsCapabilityBatchAtomically(t *testing.T) {
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		switch modelCalls {
		case 1:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"propose-1","name":"propose_capabilities","input":{"capabilities":[{"id":"list","claim":"saved inklings can be listed","scenario":"GET /inklings"},{"id":"invalid","claim":"invalid proposal","scenario":""}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"finish-1","name":"finish_examination","input":{}}]}`))
		default:
			t.Errorf("unexpected model call %d", modelCalls)
		}
	}))
	defer model.Close()

	inputDir, outputDir := agentDirectories(t)
	t.Setenv(AnthropicAPIKeyEnvVar, "test-key")
	err := RunAgent(context.Background(), AgentOptions{
		InputDir: inputDir, OutputDir: outputDir, AppURL: "http://127.0.0.1:8080", Model: "test-model", APIBaseURL: model.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "proposed no capabilities") {
		t.Fatalf("RunAgent() error = %v, want rejected batch to record no capabilities", err)
	}
}

func TestRunAgent_RejectsAssessmentBatchAtomically(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"one","text":"hello"}]`))
	}))
	defer app.Close()

	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		switch modelCalls {
		case 1:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"propose-1","name":"propose_capabilities","input":{"capabilities":[{"id":"list","claim":"saved inklings can be listed","scenario":"GET /inklings"}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"drive-1","name":"drive_app","input":{"capabilityId":"list","request":{"method":"GET","path":"/inklings"}}}]}`))
		case 3:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"assess-1","name":"assess_capabilities","input":{"assessments":[{"capabilityId":"list","verdict":"confirmed","evidence":"the response listed the saved inkling"},{"capabilityId":"unknown","verdict":"confirmed","evidence":"invalid assessment"}]}}]}`))
		case 4:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"finish-1","name":"finish_examination","input":{}}]}`))
		default:
			t.Errorf("unexpected model call %d", modelCalls)
		}
	}))
	defer model.Close()

	inputDir, outputDir := agentDirectories(t)
	t.Setenv(AnthropicAPIKeyEnvVar, "test-key")
	if err := RunAgent(context.Background(), AgentOptions{
		InputDir: inputDir, OutputDir: outputDir, AppURL: app.URL, Model: "test-model", APIBaseURL: model.URL,
	}); err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outputDir, VerdictName))
	if err != nil {
		t.Fatalf("reading verdict: %v", err)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decoding verdict: %v", err)
	}
	if result.Kind != Refused || len(result.Verdict.Outcomes) != 1 || result.Verdict.Outcomes[0].Verdict != CouldNotBeTested {
		t.Fatalf("verdict = %#v, want rejected batch to leave capability unassessed", result)
	}
}

func TestDriveApp_RejectsRedirectToAnotherOrigin(t *testing.T) {
	destinationCalls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls++
	}))
	defer destination.Close()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/private", http.StatusFound)
	}))
	defer app.Close()
	base, err := validAppURL(app.URL)
	if err != nil {
		t.Fatalf("validAppURL() error = %v", err)
	}
	request := AppRequest{Method: http.MethodGet, Path: "/redirect"}

	if _, err := driveApp(context.Background(), base, request); err == nil {
		t.Fatal("driveApp() followed a redirect outside the configured app origin")
	}
	if destinationCalls != 0 {
		t.Fatalf("redirect destination received %d calls, want none", destinationCalls)
	}
}

func agentDirectories(t *testing.T) (string, string) {
	t.Helper()
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	files := map[string]string{
		InputRequestName:      "A user can list saved inklings.",
		InputFeatureMapName:   "GET /inklings returns saved inklings.",
		InputAlwaysTrueName:   "Saved inklings remain listable.",
		InputChangedFilesName: `{"baseCommit":"abc123","files":[]}`,
		InputBaseTestsName:    `{"baseCommit":"abc123","tests":[]}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(inputDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return inputDir, outputDir
}
