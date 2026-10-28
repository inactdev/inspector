package alwaystrue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

// driveHTTP against a real server, with no Docker: the http falsifier is
// performed by Inspector itself, so this is the whole of that path.
func TestDriveHTTP_HoldsWhenEveryStepAnswersAsPromised(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"abc"}`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"abc","text":"kept"}]`))
	}))
	defer server.Close()

	promise := Promise{ID: "never-lost", Falsify: &Falsify{HTTP: []HTTPStep{
		{Method: http.MethodPost, Path: "/things", Expect: Expect{Status: 201, BodyContains: []string{"abc"}}},
		{Method: http.MethodGet, Path: "/things", Expect: Expect{Status: 200, BodyContains: []string{"kept"}}},
	}}}
	var result PromiseResult
	driveHTTP(context.Background(), promise, &runningApp{HostURL: server.URL}, &result)

	if result.Outcome != Held {
		t.Fatalf("outcome = %q (%s), want %q", result.Outcome, result.Reason, Held)
	}
	if len(seen) != 2 {
		t.Fatalf("requests = %v, want both steps driven", seen)
	}
	if !strings.Contains(result.Evidence, "POST /things -> 201") {
		t.Fatalf("evidence = %q, want what was actually observed", result.Evidence)
	}
}

func TestDriveHTTP_BreaksOnTheFirstStepThatDoesNotMeetItsExpectation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	promise := Promise{ID: "non-uuid-rejected", Falsify: &Falsify{HTTP: []HTTPStep{
		{Method: http.MethodPost, Path: "/things", Expect: Expect{Status: 400}},
	}}}
	var result PromiseResult
	driveHTTP(context.Background(), promise, &runningApp{HostURL: server.URL}, &result)

	if result.Outcome != Broken {
		t.Fatalf("outcome = %q, want %q", result.Outcome, Broken)
	}
	if !strings.Contains(result.Reason, "answered 500, not the promised 400") {
		t.Fatalf("reason = %q, want it to name what broke", result.Reason)
	}
}

// A body expectation is the half that catches a response with the right status
// and the wrong content - a duplicate left behind by an upsert, for instance.
func TestDriveHTTP_BodyExpectationsDecideBeyondTheStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"text":"first wording"},{"text":"second wording"}]`))
	}))
	defer server.Close()

	promise := Promise{ID: "upsert", Falsify: &Falsify{HTTP: []HTTPStep{{
		Method: http.MethodGet, Path: "/things",
		Expect: Expect{Status: 200, BodyNotContains: []string{"first wording"}},
	}}}}
	var result PromiseResult
	driveHTTP(context.Background(), promise, &runningApp{HostURL: server.URL}, &result)

	if result.Outcome != Broken || !strings.Contains(result.Reason, "still contains") {
		t.Fatalf("outcome = %q reason = %q, want a break naming the leftover content", result.Outcome, result.Reason)
	}
}

// A request that never reached the app says nothing about the product, so it
// is reported as not checked rather than as a broken promise. That is what
// keeps an unreachable app out of the red state.
func TestDriveHTTP_AnUndeliveredRequestIsNotABrokenPromise(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()

	promise := Promise{ID: "unreachable", Falsify: &Falsify{HTTP: []HTTPStep{
		{Method: http.MethodGet, Path: "/things", Expect: Expect{Status: 200}},
	}}}
	var result PromiseResult
	driveHTTP(context.Background(), promise, &runningApp{HostURL: url}, &result)

	if result.Outcome != NotRun {
		t.Fatalf("outcome = %q (%s), want %q", result.Outcome, result.Reason, NotRun)
	}
}

// A redirect is not the promised status. Following one would decide the
// expectation against a different resource, and off-origin a different server.
func TestDriveHTTP_DoesNotFollowARedirectToDecideAnExpectation(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("the promised body, from the wrong place"))
	}))
	defer elsewhere.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/things", http.StatusFound)
	}))
	defer server.Close()

	promise := Promise{ID: "redirect", Falsify: &Falsify{HTTP: []HTTPStep{{
		Method: http.MethodGet, Path: "/things",
		Expect: Expect{Status: 200, BodyContains: []string{"the promised body"}},
	}}}}
	var result PromiseResult
	driveHTTP(context.Background(), promise, &runningApp{HostURL: server.URL}, &result)

	if result.Outcome != Broken {
		t.Fatalf("outcome = %q, want a redirect to break the expectation rather than satisfy it", result.Outcome)
	}
}

func TestDrive_ALineWithNoFalsifierIsNotRunnableAndSaysSo(t *testing.T) {
	var result PromiseResult
	result.Shape = "none"
	drive(context.Background(), List{}, Promise{ID: "prose-only"}, nil, "", container.Limits{}, &result)
	if result.Outcome != NotRunnable {
		t.Fatalf("outcome = %q, want %q", result.Outcome, NotRunnable)
	}
	if !strings.Contains(result.Reason, "never reported as passing") {
		t.Fatalf("reason = %q, want it to say outright that this is not a pass", result.Reason)
	}
}

func TestDrive_ALineWhoseAppNeverCameUpIsNotRunRatherThanBroken(t *testing.T) {
	list := List{App: &App{Image: "i", Port: 1, Ready: Ready{Path: "/", Status: 200}}}
	promise := Promise{ID: "needs-app", Falsify: &Falsify{HTTP: []HTTPStep{
		{Method: http.MethodGet, Path: "/", Expect: Expect{Status: 200}},
	}}}
	result := PromiseResult{Shape: promise.Shape()}
	drive(context.Background(), list, promise, nil, "", container.Limits{}, &result)
	if result.Outcome != NotRun {
		t.Fatalf("outcome = %q, want %q", result.Outcome, NotRun)
	}
}

// Run never reaches Docker for a list it cannot read, so this holds with or
// without a container runtime present.
func TestRun_AnUnreadableListRefusesBeforeStartingAnything(t *testing.T) {
	result, err := Run(context.Background(), Options{ListPath: filepath.Join(t.TempDir(), "absent.json"), Timeout: time.Minute})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Kind != Refused {
		t.Fatalf("kind = %q, want %q", result.Kind, Refused)
	}
	if result.AppStarted {
		t.Fatal("nothing may be started for a list that could not be read")
	}
}

func TestWriteFixture_CreatesTheDeclaredDataAndNothingElse(t *testing.T) {
	dir, err := writeFixture(&Fixture{Dir: "/fixture", Files: map[string]string{
		"storage/seed.md": "# seeded",
		"top.txt":         "",
	}})
	if err != nil {
		t.Fatalf("writeFixture: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	content, err := os.ReadFile(filepath.Join(dir, "storage", "seed.md"))
	if err != nil || string(content) != "# seeded" {
		t.Fatalf("seed file = %q, err = %v", content, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("fixture holds %d top-level entries, want exactly the two declared", len(entries))
	}
}

// `docker port` answers either "127.0.0.1:49155" or
// "8080/tcp -> 127.0.0.1:49155", and lists a second line on a dual-stack
// host. Both forms have to be read, or the app comes up and is then declared
// unreachable.
func TestParsePublishedPort_ReadsEitherFormDockerPrints(t *testing.T) {
	cases := map[string]string{
		"bare mapping":  "127.0.0.1:49155\n",
		"arrow mapping": "8080/tcp -> 127.0.0.1:49155\n",
		"blank lines":   "\n127.0.0.1:49155\n",
		"dual stack":    "8080/tcp -> 127.0.0.1:49155\n8080/tcp -> [::1]:49155\n",
	}
	for name, output := range cases {
		got, err := parsePublishedPort(output)
		if err != nil || got != "49155" {
			t.Errorf("%s: parsed port = %q, err = %v, want 49155", name, got, err)
		}
	}
	if _, err := parsePublishedPort("\n"); err == nil {
		t.Error("an empty answer must be an error, not a port nothing is listening on")
	}
}
