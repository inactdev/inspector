package inspector

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestFindExistingPullRequest_NoneFound(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	prURL, found, err := FindExistingPullRequest(PullRequestOptions{
		Owner: "inactdev", Repo: "inspector",
		Head: "feature", Base: "main",
		Token: "test-token", APIBaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found || prURL != "" {
		t.Fatalf("found = %v, url = %q, want false and empty", found, prURL)
	}
	if gotPath != "/repos/inactdev/inspector/pulls" {
		t.Fatalf("path = %q, want /repos/inactdev/inspector/pulls", gotPath)
	}
	query, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parsing query %q: %v", gotQuery, err)
	}
	if query.Get("head") != "inactdev:feature" {
		t.Fatalf("head query = %q, want %q", query.Get("head"), "inactdev:feature")
	}
	if query.Get("base") != "main" {
		t.Fatalf("base query = %q, want main", query.Get("base"))
	}
}

func TestFindExistingPullRequest_Found(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"html_url":"https://github.com/inactdev/inspector/pull/7"}]`))
	}))
	defer server.Close()

	prURL, found, err := FindExistingPullRequest(PullRequestOptions{
		Owner: "inactdev", Repo: "inspector",
		Head: "feature", Base: "main",
		Token: "test-token", APIBaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found || prURL != "https://github.com/inactdev/inspector/pull/7" {
		t.Fatalf("found = %v, url = %q, want true and the pull request URL", found, prURL)
	}
}

func TestFindExistingPullRequest_NoToken(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	_, _, err := FindExistingPullRequest(PullRequestOptions{
		Owner: "inactdev", Repo: "inspector",
		Head: "feature", Base: "main",
		APIBaseURL: server.URL,
	})
	if err == nil {
		t.Fatal("expected an error for a missing token")
	}
	if called {
		t.Fatal("must not call GitHub without a token")
	}
}

func TestOpenPullRequest_Success(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
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
		w.Write([]byte(`{"html_url":"https://github.com/inactdev/inspector/pull/9"}`))
	}))
	defer server.Close()

	prURL, err := OpenPullRequest(PullRequestOptions{
		Owner: "inactdev", Repo: "inspector",
		Head: "feature", Base: "main",
		Title: "Implement login", Body: "the login flow is done",
		Token: "test-token", APIBaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prURL != "https://github.com/inactdev/inspector/pull/9" {
		t.Fatalf("url = %q, want the created pull request URL", prURL)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/repos/inactdev/inspector/pulls" {
		t.Errorf("path = %q, want /repos/inactdev/inspector/pulls", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-token")
	}
	if gotBody.Title != "Implement login" || gotBody.Head != "feature" || gotBody.Base != "main" || gotBody.Body != "the login flow is done" {
		t.Errorf("request body = %+v, unexpected", gotBody)
	}
}

func TestOpenPullRequest_APIRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"A pull request already exists"}`))
	}))
	defer server.Close()

	_, err := OpenPullRequest(PullRequestOptions{
		Owner: "inactdev", Repo: "inspector",
		Head: "feature", Base: "main",
		Title: "Implement login", Body: "the login flow is done",
		Token: "test-token", APIBaseURL: server.URL,
	})
	if err == nil {
		t.Fatal("expected an error for an API rejection")
	}
}
