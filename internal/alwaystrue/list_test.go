package alwaystrue

import (
	"path/filepath"
	"strings"
	"testing"
)

const minimalList = `{
  "version": 1,
  "promises": [
    {"id": "one", "promise": "this always holds", "confirmed": true}
  ]
}`

func TestParse_AcceptsAPromiseWithNoFalsifier(t *testing.T) {
	list, err := Parse([]byte(minimalList))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := list.Lines[0].Shape(); got != "none" {
		t.Fatalf("shape = %q, want none", got)
	}
}

// A declared-but-empty falsifier is the one shape that must not be tolerated:
// a line that reads as having a check but runs nothing would report as passing
// forever, which is the silent weakening this list exists to prevent.
func TestParse_RefusesAFalsifierThatChecksNothing(t *testing.T) {
	_, err := Parse([]byte(`{"version": 1, "promises": [
	  {"id": "one", "promise": "p", "falsify": {}}
	]}`))
	if err == nil || !strings.Contains(err.Error(), "declared but empty") {
		t.Fatalf("err = %v, want a refusal naming the empty falsifier", err)
	}
}

func TestParse_RefusesMoreThanOneFalsifierShape(t *testing.T) {
	_, err := Parse([]byte(`{"version": 1, "runner": {"image": "busybox"}, "promises": [
	  {"id": "one", "promise": "p", "falsify": {"command": ["true"], "script": {"path": "/x"}}}
	]}`))
	if err == nil || !strings.Contains(err.Error(), "exactly one of http, command, or script") {
		t.Fatalf("err = %v, want a refusal naming the single-shape rule", err)
	}
}

// Unknown and duplicated fields must fail closed. A misspelled expectation
// that silently decoded to nothing would leave the promise passing against an
// assertion nobody made.
func TestParse_FailsClosedOnFieldsItCannotVouchFor(t *testing.T) {
	for name, data := range map[string]string{
		"unknown field": `{"version": 1, "promises": [{"id": "a", "promise": "p", "promize": "typo"}]}`,
		"duplicate key": `{"version": 1, "version": 1, "promises": [{"id": "a", "promise": "p"}]}`,
		"unknown expectation": `{"version": 1, "app": {"image": "i", "port": 1, "ready": {"path": "/", "status": 200}},
		  "promises": [{"id": "a", "promise": "p", "falsify": {"http": [{"method": "GET", "path": "/", "expect": {"status": 200, "bodyContain": ["x"]}}]}}]}`,
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Fatalf("%s: Parse accepted a list it cannot vouch for", name)
		}
	}
}

func TestParse_RefusesAListThatChecksNothingAtAll(t *testing.T) {
	for name, data := range map[string]string{
		"empty array": `{"version": 1, "promises": []}`,
		"absent":      `{"version": 1}`,
		"null":        `{"version": 1, "promises": null}`,
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Fatalf("%s: an empty list must not be able to report that everything still holds", name)
		}
	}
}

func TestParse_RefusesAFutureFormatByNumber(t *testing.T) {
	_, err := Parse([]byte(`{"version": 2, "promises": [{"id": "a", "promise": "p"}]}`))
	if err == nil || !strings.Contains(err.Error(), "format 1 only") {
		t.Fatalf("err = %v, want a refusal naming the format version", err)
	}
}

func TestParse_RefusesDuplicateIDs(t *testing.T) {
	_, err := Parse([]byte(`{"version": 1, "promises": [
	  {"id": "same", "promise": "p"}, {"id": "same", "promise": "q"}
	]}`))
	if err == nil || !strings.Contains(err.Error(), "appears more than once") {
		t.Fatalf("err = %v, want a refusal naming the duplicate id", err)
	}
}

func TestParse_RefusesAFalsifierWithNothingToRunItAgainst(t *testing.T) {
	cases := map[string]struct{ data, want string }{
		"http without an app": {
			data: `{"version": 1, "promises": [{"id": "a", "promise": "p", "falsify": {"http": [{"method": "GET", "path": "/", "expect": {"status": 200}}]}}]}`,
			want: "declares no app",
		},
		"command without a runner image": {
			data: `{"version": 1, "promises": [{"id": "a", "promise": "p", "falsify": {"command": ["true"]}}]}`,
			want: "runner.image",
		},
	}
	for name, test := range cases {
		_, err := Parse([]byte(test.data))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: err = %v, want it to name %q", name, err, test.want)
		}
	}
}

// Check 3 mounts no repository, so a driver script can only be named by its
// path inside the runner image. A relative path would read as a project path
// and quietly resolve to the container's working directory instead.
func TestParse_RefusesAScriptPathThatIsNotInsideTheRunnerImage(t *testing.T) {
	_, err := Parse([]byte(`{"version": 1, "runner": {"image": "busybox"}, "promises": [
	  {"id": "a", "promise": "p", "falsify": {"script": {"path": "script/drive"}}}
	]}`))
	if err == nil || !strings.Contains(err.Error(), "mounts no repository") {
		t.Fatalf("err = %v, want a refusal explaining there is no repository to name a script in", err)
	}
}

func TestParse_RefusesAnAppItCannotBringUp(t *testing.T) {
	cases := map[string]string{
		"no image":        `{"version": 1, "app": {"image": "", "port": 8080, "ready": {"path": "/", "status": 200}}, "promises": [{"id": "a", "promise": "p"}]}`,
		"no port":         `{"version": 1, "app": {"image": "i", "ready": {"path": "/", "status": 200}}, "promises": [{"id": "a", "promise": "p"}]}`,
		"no ready path":   `{"version": 1, "app": {"image": "i", "port": 8080, "ready": {"path": "inklings", "status": 200}}, "promises": [{"id": "a", "promise": "p"}]}`,
		"no ready status": `{"version": 1, "app": {"image": "i", "port": 8080, "ready": {"path": "/", "status": 0}}, "promises": [{"id": "a", "promise": "p"}]}`,
		"escaping fixture": `{"version": 1, "app": {"image": "i", "port": 8080, "ready": {"path": "/", "status": 200},
		  "fixture": {"dir": "/fixture", "files": {"../escape": ""}}}, "promises": [{"id": "a", "promise": "p"}]}`,
		"relative fixture dir": `{"version": 1, "app": {"image": "i", "port": 8080, "ready": {"path": "/", "status": 200},
		  "fixture": {"dir": "fixture"}}, "promises": [{"id": "a", "promise": "p"}]}`,
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Fatalf("%s: Parse accepted an app declaration it cannot act on", name)
		}
	}
}

func TestPromiseTimeout_FallsBackRatherThanRunningUnbounded(t *testing.T) {
	if got := (Promise{}).PromiseTimeout(); got != DefaultPromiseTimeout {
		t.Fatalf("timeout = %s, want %s", got, DefaultPromiseTimeout)
	}
	if got := (Promise{Timeout: "30s"}).PromiseTimeout(); got.String() != "30s" {
		t.Fatalf("timeout = %s, want 30s", got)
	}
	if got := (Ready{}).ReadyTimeout(); got != DefaultReadyTimeout {
		t.Fatalf("ready timeout = %s, want %s", got, DefaultReadyTimeout)
	}
}

// Both lists this repository ships have to be lists this runner can actually
// read. Without this, a typo in either would only be found by running check 3
// against a live app.
func TestShippedListsAreValid(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", DefaultListName),
		filepath.Join("..", "..", "examples", "inkwell", "inkwell-always.json"),
	} {
		list, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%s): %v", path, err)
		}
		for _, promise := range list.Lines {
			if promise.Falsify == nil && strings.TrimSpace(promise.Note) == "" && strings.TrimSpace(list.Note) == "" {
				t.Errorf("%s: promise %q has no falsifier and nothing says why", path, promise.ID)
			}
		}
	}
}

// Inspector's own list is seeded by inference and no line has been accepted by
// a person yet. If that ever changes it must change deliberately, because a
// confirmed line is one the project is standing behind.
func TestInspectorsOwnListIsStillAllProposals(t *testing.T) {
	list, err := Load(filepath.Join("..", "..", DefaultListName))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, promise := range list.Lines {
		if promise.Confirmed {
			t.Errorf("promise %q is marked confirmed; a person has to accept a line before it claims to be accepted", promise.ID)
		}
		if strings.TrimSpace(promise.Source) == "" {
			t.Errorf("promise %q does not say where it came from", promise.ID)
		}
	}
}
