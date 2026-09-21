package inspector

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostCommitStatus_Success(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1,"state":"success"}`))
	}))
	defer server.Close()

	err := PostCommitStatus(PostStatusOptions{
		Owner:       "inactdev",
		Repo:        "inspector",
		Commit:      "deadbeef",
		State:       StatusSuccess,
		Description: "inspector: green",
		Token:       "test-token",
		APIBaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/repos/inactdev/inspector/statuses/deadbeef" {
		t.Errorf("path = %q, want /repos/inactdev/inspector/statuses/deadbeef", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-token")
	}
	if gotBody.State != "success" {
		t.Errorf("state = %q, want success", gotBody.State)
	}
	if gotBody.Context != StatusContext {
		t.Errorf("context = %q, want %q", gotBody.Context, StatusContext)
	}
	if gotBody.Description != "inspector: green" {
		t.Errorf("description = %q, want %q", gotBody.Description, "inspector: green")
	}
}

func TestPostCommitStatus_NoToken(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	err := PostCommitStatus(PostStatusOptions{
		Owner:      "inactdev",
		Repo:       "inspector",
		Commit:     "deadbeef",
		State:      StatusSuccess,
		Token:      "",
		APIBaseURL: server.URL,
	})
	if err == nil {
		t.Fatal("expected an error with no token")
	}
	if !strings.Contains(err.Error(), GitHubTokenEnvVar) {
		t.Fatalf("error = %q, want it to name %s", err.Error(), GitHubTokenEnvVar)
	}
	if called {
		t.Fatal("expected no request to be made without a token")
	}
}

func TestPostCommitStatus_APIRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer server.Close()

	err := PostCommitStatus(PostStatusOptions{
		Owner:      "inactdev",
		Repo:       "inspector",
		Commit:     "deadbeef",
		State:      StatusSuccess,
		Token:      "bad-token",
		APIBaseURL: server.URL,
	})
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("error = %q, want it to surface GitHub's own message", err.Error())
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %q, want it to name the status code", err.Error())
	}
	if strings.Contains(err.Error(), "outcome unconfirmed") {
		t.Fatalf("error = %q, want an explicit client rejection to be definitive", err.Error())
	}
}

func TestPostCommitStatus_ServerFailureIsUnconfirmed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"message":"upstream response lost"}`))
	}))
	defer server.Close()

	err := PostCommitStatus(PostStatusOptions{
		Owner:      "inactdev",
		Repo:       "inspector",
		Commit:     "deadbeef",
		State:      StatusSuccess,
		Token:      "test-token",
		APIBaseURL: server.URL,
	})
	if err == nil {
		t.Fatal("expected an error for a 503 response")
	}
	if !strings.Contains(err.Error(), "stamp sent, outcome unconfirmed") {
		t.Fatalf("error = %q, want ambiguous publication wording", err.Error())
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("error = %q, want it to name the status code", err.Error())
	}
}

// GitHub answers 301 for a renamed or transferred repository, and a
// followed redirect turns the POST into a GET of the "list commit
// statuses" endpoint, which answers 200 having recorded nothing. That
// must be an error, not a silently successful post.
func TestPostCommitStatus_RedirectIsNotSuccess(t *testing.T) {
	posted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
			w.Header().Set("Location", "https://api.github.com/repositories/1/statuses/deadbeef")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	err := PostCommitStatus(PostStatusOptions{
		Owner:      "inactdev",
		Repo:       "old-name",
		Commit:     "deadbeef",
		State:      StatusSuccess,
		Token:      "test-token",
		APIBaseURL: server.URL,
	})
	if err == nil {
		t.Fatal("expected an error for a redirected post - nothing was recorded")
	}
	if !posted {
		t.Fatal("expected the POST itself to have been attempted")
	}
	if !strings.Contains(err.Error(), "301") {
		t.Fatalf("error = %q, want it to name the redirect status", err.Error())
	}
}

// Only 201 Created means GitHub recorded the status; any other 2xx is a
// different endpoint answering, not a recorded result.
func TestPostCommitStatus_Non201SuccessIsNotSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	err := PostCommitStatus(PostStatusOptions{
		Owner:      "inactdev",
		Repo:       "inspector",
		Commit:     "deadbeef",
		State:      StatusSuccess,
		Token:      "test-token",
		APIBaseURL: server.URL,
	})
	if err == nil {
		t.Fatal("expected an error for a 200 response - only 201 Created records a status")
	}
}

// A commit GitHub has never seen cannot carry a status. Inspector owns that
// transfer: it stages green work before posting rather than asking a builder
// to push it, so the error must name the failed staging order.
func TestPostCommitStatus_UnavailableCommitNamesStaging(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"No commit found for SHA: deadbeef"}`))
	}))
	defer server.Close()

	err := PostCommitStatus(PostStatusOptions{
		Owner:      "inactdev",
		Repo:       "inspector",
		Commit:     "deadbeef",
		State:      StatusSuccess,
		Token:      "test-token",
		APIBaseURL: server.URL,
	})
	if err == nil {
		t.Fatal("expected an error for a commit GitHub does not have")
	}
	if !strings.Contains(err.Error(), "temporary staging ref") {
		t.Fatalf("error = %q, want it to name inspector's staging step", err.Error())
	}
	if !strings.Contains(err.Error(), "deadbeef") {
		t.Fatalf("error = %q, want it to name the commit", err.Error())
	}
}

// A 404 is repo-not-found or no-access, not an unpushed commit. It must
// keep the generic wording rather than sending someone off to push a
// commit that is already there.
func TestPostCommitStatus_NotFoundIsNotReportedAsUnpushed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	err := PostCommitStatus(PostStatusOptions{
		Owner:      "inactdev",
		Repo:       "inspector",
		Commit:     "deadbeef",
		State:      StatusSuccess,
		Token:      "test-token",
		APIBaseURL: server.URL,
	})
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if strings.Contains(err.Error(), "temporary staging ref") {
		t.Fatalf("error = %q, should not blame a staging failure for a 404", err.Error())
	}
}

func TestPostCommitStatus_NoOwnerRepo(t *testing.T) {
	err := PostCommitStatus(PostStatusOptions{
		Commit: "deadbeef",
		State:  StatusSuccess,
		Token:  "test-token",
	})
	if err == nil {
		t.Fatal("expected an error with no owner/repo")
	}
}
