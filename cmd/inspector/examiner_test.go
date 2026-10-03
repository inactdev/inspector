package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/inactdev/inspector/internal/examiner"
)

func TestExaminer_RedPostsItsOwnFailureStatusAndNamesTheCapability(t *testing.T) {
	var got struct {
		State   string `json:"state"`
		Context string `json:"context"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding status: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	withExaminerRun(t, examiner.Result{
		Kind: examiner.Red,
		Verdict: examiner.Verdict{
			Outcomes: []examiner.Outcome{
				{
					Claim: "a duplicate capture updates the existing inkling", Verdict: examiner.NotConfirmed, Scenario: "POST twice", Evidence: "two entries returned", ProposedRegression: "add an HTTP upsert regression",
				},
			},
		},
	})
	previousAPIBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = previousAPIBaseURL })
	t.Setenv("GITHUB_TOKEN", "test-token")

	var stdout, stderr bytes.Buffer
	code := run([]string{"examine", "--request", "request", "--guidebook", "guidebook", "--test-changes", "changes", "--app-url", "http://app", "--commit", "deadbeef", "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model"}, &stdout, &stderr)
	if code != exitRed {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitRed, stderr.String())
	}
	if got.State != "failure" || got.Context != "examiner" {
		t.Fatalf("posted status = %#v, want examiner failure", got)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("a duplicate capture updates the existing inkling")) {
		t.Fatalf("stdout = %q, want the missing capability", stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("proposed regression")) {
		t.Fatalf("stdout = %q, want the proposed regression", stdout.String())
	}
}

func TestExaminer_RefusalPostsErrorNotFailure(t *testing.T) {
	var got struct {
		State   string `json:"state"`
		Context string `json:"context"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding status: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	withExaminerRun(t, examiner.Result{Kind: examiner.Refused, Message: "the guidebook could not be read"})
	previousAPIBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = previousAPIBaseURL })
	t.Setenv("GITHUB_TOKEN", "test-token")

	var stdout, stderr bytes.Buffer
	code := run([]string{"examine", "--request", "request", "--guidebook", "guidebook", "--test-changes", "changes", "--app-url", "http://app", "--commit", "deadbeef", "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if got.State != "error" || got.Context != "examiner" {
		t.Fatalf("posted status = %#v, want examiner error", got)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("refused")) || bytes.Contains(stdout.Bytes(), []byte("red -")) {
		t.Fatalf("refusal output must not look red: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestExaminer_RejectsCallerSuppliedAgentBinary(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"examine", "--agent-binary", "/tmp/untrusted-agent"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("flag provided but not defined")) {
		t.Fatalf("stderr = %q, want rejected agent override", stderr.String())
	}
}

func TestExaminer_StartFailurePostsErrorStatus(t *testing.T) {
	var got struct {
		State   string `json:"state"`
		Context string `json:"context"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding status: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	previousRun := runExamination
	runExamination = func(context.Context, examiner.RunOptions) (examiner.Result, error) {
		return examiner.Result{}, errors.New("container creation failed")
	}
	t.Cleanup(func() { runExamination = previousRun })
	previousAPIBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = previousAPIBaseURL })
	t.Setenv("GITHUB_TOKEN", "test-token")

	var stdout, stderr bytes.Buffer
	code := run([]string{"examine", "--request", "request", "--guidebook", "guidebook", "--test-changes", "changes", "--app-url", "http://app", "--commit", "deadbeef", "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if got.State != "error" || got.Context != "examiner" {
		t.Fatalf("posted status = %#v, want examiner error", got)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("container creation failed")) {
		t.Fatalf("stderr = %q, want start failure", stderr.String())
	}
}

func TestExaminerAgent_UsesConfiguredTimeout(t *testing.T) {
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	for name, content := range map[string]string{
		examiner.InputRequestName:     "support duplicate captures",
		examiner.InputGuidebookName:   "POST /captures",
		examiner.InputTestChangesName: `{"baseCommit":"abc123","files":[]}`,
	} {
		if err := os.WriteFile(filepath.Join(inputDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	t.Setenv(examiner.AnthropicAPIKeyEnvVar, "test-key")

	var stdout, stderr bytes.Buffer
	code := runExaminerAgent([]string{
		"--input-dir", inputDir,
		"--output-dir", outputDir,
		"--app-url", "http://app",
		"--model", "test-model",
		"--api-base-url", "http://127.0.0.1:1",
		"--timeout", "1ns",
	}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitRefused, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("context deadline exceeded")) {
		t.Fatalf("stderr = %q, want configured timeout failure", stderr.String())
	}
}

func TestExaminer_StatusFailurePreservesLocalFindings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "status unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	withExaminerRun(t, examiner.Result{
		Kind: examiner.Red,
		Verdict: examiner.Verdict{Outcomes: []examiner.Outcome{{
			Claim: "duplicate captures update the existing inkling", Verdict: examiner.NotConfirmed,
			Scenario: "POST twice", Evidence: "two entries returned", ProposedRegression: "add an HTTP upsert regression",
		}}},
	})
	previousAPIBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = previousAPIBaseURL })
	t.Setenv("GITHUB_TOKEN", "test-token")

	var stdout, stderr bytes.Buffer
	code := run([]string{"examine", "--request", "request", "--guidebook", "guidebook", "--test-changes", "changes", "--app-url", "http://app", "--commit", "deadbeef", "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("add an HTTP upsert regression")) {
		t.Fatalf("stdout = %q, want the completed finding despite status failure", stdout.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("could not post its separate status")) {
		t.Fatalf("stderr = %q, want publication refusal", stderr.String())
	}
}

func withExaminerRun(t *testing.T, result examiner.Result) {
	t.Helper()
	previous := runExamination
	runExamination = func(context.Context, examiner.RunOptions) (examiner.Result, error) { return result, nil }
	t.Cleanup(func() { runExamination = previous })
}
