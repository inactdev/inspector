package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/alwaystrue"
)

func alwaysArgs() []string {
	return []string{
		"always", "--list", "list.json",
		"--commit", testExaminerCommit, "--owner", "inactdev", "--repo", "inkwell",
	}
}

func withAlwaysRun(t *testing.T, result alwaystrue.Result, err error) {
	t.Helper()
	previous := runAlwaysTrue
	runAlwaysTrue = func(context.Context, alwaystrue.Options) (alwaystrue.Result, error) { return result, err }
	t.Cleanup(func() { runAlwaysTrue = previous })
}

// A broken promise is a red, and the status names every one of them rather
// than stopping at the first - that is what one aggregate verdict is for.
func TestAlways_ABrokenPromisePostsFailureAndNamesAllOfThem(t *testing.T) {
	var got struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description"`
	}
	server := statusServer(t, &got)
	defer server.Close()
	withAlwaysRun(t, alwaystrue.DeriveResult("list.json", true, "", []alwaystrue.PromiseResult{
		{ID: "inkling-never-lost", Promise: "an inkling is never lost", Shape: "http", Outcome: alwaystrue.Broken, Reason: "step 2 (GET /inklings) answered 200 as promised, but its body does not contain the id"},
		{ID: "non-uuid-rejected", Promise: "no endpoint accepts a non-UUID id", Shape: "http", Outcome: alwaystrue.Broken, Reason: "step 1 (POST /inklings) answered 201, not the promised 400"},
		{ID: "ordering", Promise: "newest first", Shape: "http", Outcome: alwaystrue.Held, Confirmed: true},
	}), nil)
	withStatusServer(t, server)

	var stdout, stderr bytes.Buffer
	if code := run(alwaysArgs(), &stdout, &stderr); code != exitRed {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitRed, stderr.String())
	}
	if got.State != "failure" || got.Context != "inspector-always" {
		t.Fatalf("posted status = %#v, want an inspector-always failure", got)
	}
	for _, want := range []string{"inkling-never-lost", "non-uuid-rejected"} {
		if !strings.Contains(got.Description, want) {
			t.Errorf("status description = %q, want %q named in it", got.Description, want)
		}
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q named in it", stderr.String(), want)
		}
	}
}

// An app that would not start is not a broken promise. It posts error, and
// says which - the distinction the ruling asks for.
func TestAlways_AnAppThatWouldNotStartPostsErrorNotFailure(t *testing.T) {
	var got struct {
		State       string `json:"state"`
		Description string `json:"description"`
	}
	server := statusServer(t, &got)
	defer server.Close()
	withAlwaysRun(t, alwaystrue.DeriveResult("list.json", false, "the app would not start: starting inkwell-backend:local: No such image", []alwaystrue.PromiseResult{
		{ID: "inkling-never-lost", Shape: "http", Outcome: alwaystrue.NotRun, Reason: "the app it drives never came up, so this line was not attempted"},
	}), nil)
	withStatusServer(t, server)

	var stdout, stderr bytes.Buffer
	if code := run(alwaysArgs(), &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if got.State != "error" {
		t.Fatalf("posted state = %q, want error", got.State)
	}
	if !strings.Contains(got.Description, "the app would not start") || !strings.Contains(got.Description, "inkling-never-lost") {
		t.Fatalf("status description = %q, want it to say which failure happened and which line went unchecked", got.Description)
	}
	if !strings.Contains(stdout.String(), "No such image") {
		t.Fatalf("stdout = %q, want docker's own message kept in the record the status abbreviates", stdout.String())
	}
	if strings.Contains(stdout.String(), "red -") {
		t.Fatalf("stdout = %q, an infrastructure failure must not read as red", stdout.String())
	}
}

// The state that most needs to not read as success: nothing broke, because
// nothing ran.
func TestAlways_ANotRunnableLinePostsErrorRatherThanSuccess(t *testing.T) {
	var got struct {
		State       string `json:"state"`
		Description string `json:"description"`
	}
	server := statusServer(t, &got)
	defer server.Close()
	withAlwaysRun(t, alwaystrue.DeriveResult("list.json", false, "", []alwaystrue.PromiseResult{
		{ID: "delete-removes-the-audio-sibling", Promise: "a delete removes both", Shape: "none", Outcome: alwaystrue.NotRunnable, Reason: "no falsifier is declared"},
	}), nil)
	withStatusServer(t, server)

	var stdout, stderr bytes.Buffer
	if code := run(alwaysArgs(), &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if got.State != "error" {
		t.Fatalf("posted state = %q, want error", got.State)
	}
	if !strings.Contains(got.Description, "delete-removes-the-audio-sibling") {
		t.Fatalf("status description = %q, want the unchecked line named", got.Description)
	}
	if !strings.Contains(stderr.String(), "did not check the whole list") {
		t.Fatalf("stderr = %q, want it said loudly that the list was not fully checked", stderr.String())
	}
}

func TestAlways_EveryPromiseHoldingPostsSuccess(t *testing.T) {
	var got struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description"`
	}
	server := statusServer(t, &got)
	defer server.Close()
	withAlwaysRun(t, alwaystrue.DeriveResult("list.json", true, "", []alwaystrue.PromiseResult{
		{ID: "a", Shape: "http", Outcome: alwaystrue.Held, Confirmed: true},
		{ID: "b", Shape: "command", Outcome: alwaystrue.Held, Confirmed: true},
	}), nil)
	withStatusServer(t, server)

	var stdout, stderr bytes.Buffer
	if code := run(alwaysArgs(), &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if got.State != "success" || got.Context != "inspector-always" {
		t.Fatalf("posted status = %#v, want an inspector-always success", got)
	}
	if !strings.Contains(stdout.String(), "command: 1, http: 1") {
		t.Fatalf("stdout = %q, want what the list is made of reported", stdout.String())
	}
}

// Said on every run with a proposal in it: an unaccepted line is driven and
// counted all the same, and leaving that unstated would let a red look like it
// came only from the accepted set.
func TestAlways_SaysHowManyLinesAreStillProposals(t *testing.T) {
	var got struct {
		State string `json:"state"`
	}
	server := statusServer(t, &got)
	defer server.Close()
	withAlwaysRun(t, alwaystrue.DeriveResult("list.json", true, "", []alwaystrue.PromiseResult{
		{ID: "a", Shape: "http", Outcome: alwaystrue.Held, Confirmed: false},
		{ID: "b", Shape: "http", Outcome: alwaystrue.Held, Confirmed: true},
	}), nil)
	withStatusServer(t, server)

	var stdout, stderr bytes.Buffer
	if code := run(alwaysArgs(), &stdout, &stderr); code != exitGreen {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitGreen, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 of them are proposals no one has confirmed yet") {
		t.Fatalf("stdout = %q, want the unconfirmed count stated", stdout.String())
	}
}

func TestAlways_MissingStatusTargetRefusesWithoutRunning(t *testing.T) {
	for _, flag := range []string{"--owner", "--repo", "--commit"} {
		withAlwaysRun(t, alwaystrue.Result{}, errors.New("must not be reached"))
		var stdout, stderr bytes.Buffer
		if code := run(removeFlag(alwaysArgs(), flag), &stdout, &stderr); code != exitRefused {
			t.Errorf("%s: exit code = %d, want %d", flag, code, exitRefused)
		}
		if !strings.Contains(stderr.String(), "missing required "+flag) {
			t.Errorf("%s: stderr = %q, want the missing flag named", flag, stderr.String())
		}
	}
}

func TestAlways_RejectsAMutableStatusTarget(t *testing.T) {
	withAlwaysRun(t, alwaystrue.Result{}, errors.New("must not be reached"))
	var stdout, stderr bytes.Buffer
	args := append(removeFlag(alwaysArgs(), "--commit"), "--commit", "main")
	if code := run(args, &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "full 40-character commit SHA") {
		t.Fatalf("stderr = %q, want a refusal naming the commit requirement", stderr.String())
	}
}

func TestAlways_RejectsClaimText(t *testing.T) {
	withAlwaysRun(t, alwaystrue.Result{}, errors.New("must not be reached"))
	var stdout, stderr bytes.Buffer
	if code := run(append(alwaysArgs(), "the work is finished"), &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stderr.String(), "does not accept claim text") {
		t.Fatalf("stderr = %q, want the refusal to say where the list comes from", stderr.String())
	}
}

func TestAlways_HelpIsAUsageErrorNotAVerdict(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"always", "--help"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "inspector-always") {
		t.Fatalf("usage = %q, want it to name the status it posts", stderr.String())
	}
}

func TestAlways_RunFailureRefusesAndPostsError(t *testing.T) {
	var got struct {
		State string `json:"state"`
	}
	server := statusServer(t, &got)
	defer server.Close()
	withAlwaysRun(t, alwaystrue.Result{}, errors.New("docker daemon unreachable"))
	withStatusServer(t, server)

	var stdout, stderr bytes.Buffer
	if code := run(alwaysArgs(), &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if got.State != "error" || !strings.Contains(stderr.String(), "docker daemon unreachable") {
		t.Fatalf("state = %q stderr = %q, want an error status naming the failure", got.State, stderr.String())
	}
}

// A result that was reached but could not be recorded must not be reported as
// if it had been posted.
func TestAlways_StatusFailureRefusesWithTheLocalResultIntact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "status unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	withAlwaysRun(t, alwaystrue.DeriveResult("list.json", true, "", []alwaystrue.PromiseResult{
		{ID: "inkling-never-lost", Promise: "an inkling is never lost", Shape: "http", Outcome: alwaystrue.Broken, Reason: "answered 500"},
	}), nil)
	withStatusServer(t, server)

	var stdout, stderr bytes.Buffer
	if code := run(alwaysArgs(), &stdout, &stderr); code != exitRefused {
		t.Fatalf("exit code = %d, want %d", code, exitRefused)
	}
	if !strings.Contains(stdout.String(), "inkling-never-lost") {
		t.Fatalf("stdout = %q, want the broken promise kept despite the status failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "could not post its own status") {
		t.Fatalf("stderr = %q, want it to say the status was not recorded", stderr.String())
	}
}
