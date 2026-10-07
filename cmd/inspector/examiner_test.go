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

func TestExaminer_RedPostsFailureAndNamesIncompleteExamination(t *testing.T) {
	var got struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description"`
	}
	server := statusServer(t, &got)
	defer server.Close()
	withExaminerRun(t, examiner.Result{Kind: examiner.Red, Verdict: examiner.Verdict{
		ExaminationIncomplete: true,
		Outcomes: []examiner.Outcome{
			{Claim: "a duplicate capture updates the existing inkling", Verdict: examiner.NotConfirmed, Scenario: "POST twice", Evidence: "two entries returned", ProposedRegression: "add an HTTP upsert regression"},
			{Claim: "captures can be listed", Verdict: examiner.CouldNotBeTested, Scenario: "GET /inklings", Evidence: "app stopped"},
		},
	}})
	withStatusServer(t, server)

	var stdout, stderr bytes.Buffer
	code := run(examinerArgs(), &stdout, &stderr)
	if code != exitRed {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitRed, stderr.String())
	}
	if got.State != "failure" || got.Context != "examiner" {
		t.Fatalf("posted status = %#v, want examiner failure", got)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("a duplicate capture updates the existing inkling")) {
		t.Fatalf("stdout = %q, want the missing capability", stdout.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("examination incomplete")) || !bytes.Contains([]byte(got.Description), []byte("incomplete")) {
		t.Fatalf("incomplete examination must remain visible: stdout=%q stderr=%q status=%q", stdout.String(), stderr.String(), got.Description)
	}
}

func TestExaminer_IncompleteWithoutFindingPostsError(t *testing.T) {
	var got struct {
		State   string `json:"state"`
		Context string `json:"context"`
	}
	server := statusServer(t, &got)
	defer server.Close()
	withExaminerRun(t, examiner.Result{Kind: examiner.Refused, Verdict: examiner.Verdict{
		ExaminationIncomplete: true,
		Outcomes:              []examiner.Outcome{{Claim: "capture saves", Verdict: examiner.CouldNotBeTested, Scenario: "POST /inklings", Evidence: "app unavailable"}},
	}})
	withStatusServer(t, server)

	var stdout, stderr bytes.Buffer
	code := run(examinerArgs(), &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if got.State != "error" || got.Context != "examiner" {
		t.Fatalf("posted status = %#v, want examiner error", got)
	}
	if bytes.Contains(stdout.Bytes(), []byte("red -")) {
		t.Fatalf("incomplete-only result must not read as red: stdout=%q", stdout.String())
	}
}

func TestExaminer_MissingOperationalInputPostsErrorStatus(t *testing.T) {
	var got struct {
		State   string `json:"state"`
		Context string `json:"context"`
	}
	server := statusServer(t, &got)
	defer server.Close()
	previousRun := runExamination
	runCalled := false
	runExamination = func(context.Context, examiner.RunOptions) (examiner.Result, error) {
		runCalled = true
		return examiner.Result{Kind: examiner.Green}, nil
	}
	t.Cleanup(func() { runExamination = previousRun })
	withStatusServer(t, server)

	args := examinerArgs()
	args = removeFlag(args, "--feature-map")
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if runCalled {
		t.Fatal("examination ran without its feature map")
	}
	if got.State != "error" || got.Context != "examiner" {
		t.Fatalf("posted status = %#v, want examiner error", got)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("missing required --feature-map")) {
		t.Fatalf("stderr = %q, want missing feature-map refusal", stderr.String())
	}
}

func TestExaminer_RejectsMutableStatusTarget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := examinerArgs()
	for n, arg := range args {
		if arg == "--commit" {
			args[n+1] = "pull-request-branch"
		}
	}
	code := run(args, &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("full 40-character commit SHA")) {
		t.Fatalf("stderr = %q, want immutable commit refusal", stderr.String())
	}
}

func TestExaminer_StatusFailurePreservesLocalFindings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "status unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	withExaminerRun(t, examiner.Result{Kind: examiner.Red, Verdict: examiner.Verdict{Outcomes: []examiner.Outcome{{
		Claim: "duplicate captures update the existing inkling", Verdict: examiner.NotConfirmed,
		Scenario: "POST twice", Evidence: "two entries returned", ProposedRegression: "add an HTTP upsert regression",
	}}}})
	withStatusServer(t, server)

	var stdout, stderr bytes.Buffer
	code := run(examinerArgs(), &stdout, &stderr)
	if code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("add an HTTP upsert regression")) {
		t.Fatalf("stdout = %q, want the completed finding despite status failure", stdout.String())
	}
}

func examinerArgs() []string {
	return []string{
		"examine", "--request", "request", "--feature-map", "feature-map", "--always-true", "always-true",
		"--changed-files", "changed-files", "--base-tests", "base-tests", "--app-url", "http://app",
		"--commit", testExaminerCommit, "--owner", "inactdev", "--repo", "inkwell", "--model", "test-model",
	}
}

func removeFlag(args []string, flag string) []string {
	for n, arg := range args {
		if arg == flag {
			return append(args[:n:n], args[n+2:]...)
		}
	}
	return args
}

func statusServer(t *testing.T, target any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(target); err != nil {
			t.Errorf("decoding status: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
}

func withStatusServer(t *testing.T, server *httptest.Server) {
	t.Helper()
	previousAPIBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = previousAPIBaseURL })
	t.Setenv("GITHUB_TOKEN", "test-token")
}

func withExaminerRun(t *testing.T, result examiner.Result) {
	t.Helper()
	previous := runExamination
	runExamination = func(context.Context, examiner.RunOptions) (examiner.Result, error) { return result, nil }
	t.Cleanup(func() { runExamination = previous })
}

func TestExaminer_StartFailurePostsErrorStatus(t *testing.T) {
	var got struct {
		State string `json:"state"`
	}
	server := statusServer(t, &got)
	defer server.Close()
	previousRun := runExamination
	runExamination = func(context.Context, examiner.RunOptions) (examiner.Result, error) {
		return examiner.Result{}, errors.New("container creation failed")
	}
	t.Cleanup(func() { runExamination = previousRun })
	withStatusServer(t, server)
	var stdout, stderr bytes.Buffer
	if code := run(examinerArgs(), &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if got.State != "error" || !bytes.Contains(stderr.Bytes(), []byte("container creation failed")) {
		t.Fatalf("start failure was not posted as error: state=%q stderr=%q", got.State, stderr.String())
	}
}
