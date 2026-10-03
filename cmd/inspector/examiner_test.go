package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func withExaminerRun(t *testing.T, result examiner.Result) {
	t.Helper()
	previous := runExamination
	runExamination = func(context.Context, examiner.RunOptions) (examiner.Result, error) { return result, nil }
	t.Cleanup(func() { runExamination = previous })
}
