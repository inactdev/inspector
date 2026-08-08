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
