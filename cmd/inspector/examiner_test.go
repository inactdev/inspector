package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inactdev/inspector/internal/examiner"
)

const testExaminerCommit = "0123456789abcdef0123456789abcdef01234567"

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
	code := run([]string{"examine", "--request", "request", "--guidebook", "guidebook", "--test-changes", "changes", "--app-url", "http://app", "--commit", testExaminerCommit, "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model"}, &stdout, &stderr)
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
	code := run([]string{"examine", "--request", "request", "--guidebook", "guidebook", "--test-changes", "changes", "--app-url", "http://app", "--commit", testExaminerCommit, "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model"}, &stdout, &stderr)
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

func TestExaminer_MissingOperationalInputPostsErrorStatus(t *testing.T) {
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
	runCalled := false
	runExamination = func(context.Context, examiner.RunOptions) (examiner.Result, error) {
		runCalled = true
		return examiner.Result{Kind: examiner.Green}, nil
	}
	t.Cleanup(func() { runExamination = previousRun })
	previousAPIBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = previousAPIBaseURL })
	t.Setenv("GITHUB_TOKEN", "test-token")

	var stdout, stderr bytes.Buffer
	code := run([]string{"examine", "--request", "request", "--test-changes", "changes", "--app-url", "http://app", "--commit", testExaminerCommit, "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if runCalled {
		t.Fatal("examination ran without its guidebook")
	}
	if got.State != "error" || got.Context != "examiner" {
		t.Fatalf("posted status = %#v, want examiner error", got)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("missing required --guidebook")) {
		t.Fatalf("stderr = %q, want missing guidebook refusal", stderr.String())
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

func TestExaminer_RejectsMutableStatusTarget(t *testing.T) {
	runCalled := false
	previousRun := runExamination
	runExamination = func(context.Context, examiner.RunOptions) (examiner.Result, error) {
		runCalled = true
		return examiner.Result{Kind: examiner.Green}, nil
	}
	t.Cleanup(func() { runExamination = previousRun })

	var stdout, stderr bytes.Buffer
	code := run([]string{"examine", "--request", "request", "--guidebook", "guidebook", "--test-changes", "changes", "--app-url", "http://app", "--commit", "pull-request-branch", "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if runCalled {
		t.Fatal("examination ran for a mutable status target")
	}
	if !bytes.Contains(stderr.Bytes(), []byte("full 40-character commit SHA")) {
		t.Fatalf("stderr = %q, want immutable commit refusal", stderr.String())
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
	code := run([]string{"examine", "--request", "request", "--guidebook", "guidebook", "--test-changes", "changes", "--app-url", "http://app", "--commit", testExaminerCommit, "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model"}, &stdout, &stderr)
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

func TestExaminerAgentIsNotAPublicSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"examiner-agent"}, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("missing required --branch")) {
		t.Fatalf("stderr = %q, want the ordinary inspector usage path", stderr.String())
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
	code := run([]string{"examine", "--request", "request", "--guidebook", "guidebook", "--test-changes", "changes", "--app-url", "http://app", "--commit", testExaminerCommit, "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model"}, &stdout, &stderr)
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
