package examiner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRunAgent_DerivesAndDrivesAnHTTPScenario(t *testing.T) {
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
		var request anthropicMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decoding model request: %v", err)
		}
		switch modelCalls {
		case 1:
			if len(request.Messages) != 1 {
				t.Errorf("initial model messages = %d, want 1", len(request.Messages))
			}
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"drive-1","name":"drive_app","input":{"method":"GET","path":"/inklings"}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"verdict-1","name":"submit_verdict","input":{"outcomes":[{"claim":"saved inklings can be listed","verdict":"confirmed","scenario":"GET /inklings","evidence":"200 response contained the saved inkling"}]}}]}`))
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
	if result.Kind != Green || result.Verdict.Outcomes[0].Claim != "saved inklings can be listed" {
		t.Fatalf("verdict = %#v, want a confirmed request-derived outcome", result)
	}
}

func TestRunAgent_RequiresDrivingTheAppBeforeSubmitting(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"verdict-1","name":"submit_verdict","input":{"outcomes":[{"claim":"capture saves","verdict":"confirmed","scenario":"POST /inklings","evidence":"claimed"}]}}]}`))
	}))
	defer model.Close()

	inputDir, outputDir := agentDirectories(t)
	t.Setenv(AnthropicAPIKeyEnvVar, "test-key")
	err := RunAgent(context.Background(), AgentOptions{
		InputDir: inputDir, OutputDir: outputDir, AppURL: "http://127.0.0.1:8080", Model: "test-model", APIBaseURL: model.URL,
	})
	if err == nil {
		t.Fatal("RunAgent() accepted a verdict without operating the app")
	}
}

func TestRunAgent_RequiresObservingSuccessfulDriveResult(t *testing.T) {
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		if modelCalls == 1 {
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"drive-1","name":"drive_app","input":{"method":"GET","path":"http://other.example"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"verdict-1","name":"submit_verdict","input":{"outcomes":[{"claim":"capture saves","verdict":"confirmed","scenario":"GET /inklings","evidence":"claimed"}]}}]}`))
	}))
	defer model.Close()

	inputDir, outputDir := agentDirectories(t)
	t.Setenv(AnthropicAPIKeyEnvVar, "test-key")
	err := RunAgent(context.Background(), AgentOptions{
		InputDir: inputDir, OutputDir: outputDir, AppURL: "http://127.0.0.1:8080", Model: "test-model", APIBaseURL: model.URL,
	})
	if err == nil {
		t.Fatal("RunAgent() accepted a verdict after only a failed driver call")
	}
}

func TestRunAgent_RejectsVerdictAlongsideFirstDrive(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer app.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"drive-1","name":"drive_app","input":{"method":"GET","path":"/inklings"}},{"type":"tool_use","id":"verdict-1","name":"submit_verdict","input":{"outcomes":[{"claim":"saved inklings can be listed","verdict":"confirmed","scenario":"GET /inklings","evidence":"claimed"}]}}]}`))
	}))
	defer model.Close()

	inputDir, outputDir := agentDirectories(t)
	t.Setenv(AnthropicAPIKeyEnvVar, "test-key")
	err := RunAgent(context.Background(), AgentOptions{
		InputDir: inputDir, OutputDir: outputDir, AppURL: app.URL, Model: "test-model", APIBaseURL: model.URL,
	})
	if err == nil {
		t.Fatal("RunAgent() accepted a verdict before the model observed the driver result")
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
	input, err := json.Marshal(appRequest{Method: http.MethodGet, Path: "/redirect"})
	if err != nil {
		t.Fatalf("encoding driver call: %v", err)
	}

	if _, err := driveApp(context.Background(), base, input); err == nil {
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
		InputRequestName:     "A user can list saved inklings.",
		InputGuidebookName:   "GET /inklings returns saved inklings.",
		InputTestChangesName: `{"baseCommit":"abc123","files":[]}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(inputDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return inputDir, outputDir
}
