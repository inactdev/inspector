// Package alwaystrue owns the project's regression list - the always-true
// list - and check 3, the runner that drives every line in it.
//
// The list is one file in the judged project, under its protected paths. Each
// line carries two halves: a plain-language promise a person can read, and the
// console command, driver script, or HTTP exchange that would falsify it. The
// prose is what the list is for; the falsifier is what makes it a check. A line
// with no falsifier is reported as not-runnable and is never reported as
// passed.
//
// See internal/alwaystrue/README.md, README.md's "Always-true list" section,
// notes/20261025-check-3-the-regression-list.md, and issue #24.
package alwaystrue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

// DefaultListName is where check 3 looks for the list when no --list is
// given. Named to match the status context it posts under, and to sit beside
// .inspector.json rather than inside the gitignored .inspector/ report
// directory. A project adds it to .inspector.json's protectedPaths so a change
// to it cannot pass unseen.
const DefaultListName = ".inspector-always.json"

// FormatVersion is the only list version this runner accepts. A list from a
// future format is refused by number rather than read optimistically: a field
// this code does not know about is a promise it would not check.
const FormatVersion = 1

// maxListBytes bounds the list file, the same order of magnitude the examiner
// allows for each of its inputs.
const maxListBytes = 1 << 20

// DefaultPromiseTimeout bounds one falsifier - one HTTP exchange or one
// command container - so a single hung line cannot eat the whole run's budget
// and leave the rest of the list unattempted.
const DefaultPromiseTimeout = 2 * time.Minute

// DefaultReadyTimeout bounds waiting for the app to answer its declared
// readiness request.
const DefaultReadyTimeout = 60 * time.Second

// List is the whole regression list.
type List struct {
	Version int `json:"version"`
	// App declares how to bring the project's app up for the run. Omitted,
	// nothing is started and only command and script falsifiers can run - a
	// project whose product is a command-line tool rather than a server.
	App *App `json:"app,omitempty"`
	// Runner is the image command and script falsifiers run in. It is never
	// the repository: check 3 mounts no repository, so a driver script is a
	// path inside this image.
	Runner *Runner   `json:"runner,omitempty"`
	Note   string    `json:"note,omitempty"`
	Lines  []Promise `json:"promises"`
}

// App is the app check 3 brings up itself, with its own fixture data, and
// removes when the run ends.
type App struct {
	// Image must already be present locally. Check 3 never builds or pulls
	// while it holds app access, the same stance the examiner's runtime takes.
	Image string `json:"image"`
	// Args are appended to the image's own entrypoint.
	Args []string          `json:"args,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	// Port is the TCP port the app listens on inside its container.
	Port    int      `json:"port"`
	Fixture *Fixture `json:"fixture,omitempty"`
	Ready   Ready    `json:"ready"`
}

// Fixture is the throwaway data directory check 3 creates for the app, writes
// the declared files into, mounts writable, and deletes afterwards. It is the
// only writable path the app container gets besides its own /tmp.
type Fixture struct {
	// Dir is an absolute path inside the container.
	Dir string `json:"dir"`
	// Files maps a clean relative path under Dir to its literal content.
	Files map[string]string `json:"files,omitempty"`
}

// Ready is the request that decides the app is up. Check 3 refuses to drive a
// single promise until the app has answered it.
type Ready struct {
	Path   string `json:"path"`
	Status int    `json:"status"`
	// Timeout is a Go duration; empty means DefaultReadyTimeout.
	Timeout string `json:"timeout,omitempty"`
}

// Runner is the image a command or script falsifier runs in.
type Runner struct {
	Image string `json:"image"`
}

// Promise is one line of the list.
type Promise struct {
	ID string `json:"id"`
	// Promise is the plain-language statement of what must always be true.
	Promise string `json:"promise"`
	// Confirmed records whether a person has accepted this line. A proposed
	// line sits here with false until someone does. It is reported and
	// counted, and deliberately does not change the verdict.
	Confirmed bool `json:"confirmed"`
	// Source says where the line came from - a test it was inferred from, or
	// the check-2 finding that proposed it.
	Source string `json:"source,omitempty"`
	Note   string `json:"note,omitempty"`
	// Timeout is a Go duration bounding this line alone; empty means
	// DefaultPromiseTimeout.
	Timeout string `json:"timeout,omitempty"`
	// Falsify is what would disprove the promise. Absent, the line is
	// not-runnable - reported as such, never as passed.
	Falsify *Falsify `json:"falsify,omitempty"`
}

// Falsify holds exactly one of the three shapes the ruling names: a console
// command, a driver script, or HTTP calls. More than one, or none, is refused
// when the list is read rather than at the point of running it.
type Falsify struct {
	HTTP    []HTTPStep `json:"http,omitempty"`
	Command []string   `json:"command,omitempty"`
	Script  *Script    `json:"script,omitempty"`
}

// Script names a driver script by its absolute path inside the runner image.
// It cannot name a path in the repository: there is no repository mount.
type Script struct {
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
}

// HTTPStep is one request in a falsifying exchange. Every step must meet its
// own expectation; the first that does not breaks the promise.
type HTTPStep struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Expect  Expect            `json:"expect"`
}

// Expect is what the app must answer for the step to hold.
type Expect struct {
	Status          int      `json:"status"`
	BodyContains    []string `json:"bodyContains,omitempty"`
	BodyNotContains []string `json:"bodyNotContains,omitempty"`
}

// Shape names which of the three falsifier shapes a line uses, or "none".
func (p Promise) Shape() string {
	switch {
	case p.Falsify == nil:
		return "none"
	case len(p.Falsify.HTTP) > 0:
		return "http"
	case len(p.Falsify.Command) > 0:
		return "command"
	case p.Falsify.Script != nil:
		return "script"
	default:
		return "none"
	}
}

// PromiseTimeout is how long this line's falsifier may take.
func (p Promise) PromiseTimeout() time.Duration {
	if d, err := time.ParseDuration(p.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultPromiseTimeout
}

// ReadyTimeout is how long the app has to answer its readiness request.
func (r Ready) ReadyTimeout() time.Duration {
	if d, err := time.ParseDuration(r.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultReadyTimeout
}

// Load reads and validates the list at path.
func Load(path string) (List, error) {
	data, err := readListFile(path)
	if err != nil {
		return List{}, err
	}
	list, err := Parse(data)
	if err != nil {
		return List{}, fmt.Errorf("reading the always-true list %s: %w", path, err)
	}
	return list, nil
}

func readListFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("no always-true list path was provided")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the always-true list %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading the always-true list %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("reading the always-true list %s: not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxListBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the always-true list %s: %w", path, err)
	}
	if len(data) > maxListBytes {
		return nil, fmt.Errorf("reading the always-true list %s: exceeds the %d byte limit", path, maxListBytes)
	}
	return data, nil
}

// Parse validates the list and fails closed on everything it cannot vouch for.
// Unknown and duplicated fields are refused rather than ignored: a misspelled
// expectation that decoded to nothing would leave a promise passing against an
// assertion nobody made, which is the exact silent weakening this list exists
// to prevent.
func Parse(data []byte) (List, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return List{}, err
	}
	if err := requireJSONArrayField(data, "promises"); err != nil {
		return List{}, err
	}
	var list List
	if err := decodeStrictJSON(data, &list); err != nil {
		return List{}, err
	}
	if list.Version != FormatVersion {
		return List{}, fmt.Errorf("version is %d; this inspector reads always-true list format %d only", list.Version, FormatVersion)
	}
	if len(list.Lines) == 0 {
		return List{}, errors.New("promises is empty; a list that checks nothing must not report that everything still holds")
	}
	if err := validateApp(list.App); err != nil {
		return List{}, err
	}
	if list.Runner != nil && strings.TrimSpace(list.Runner.Image) == "" {
		return List{}, errors.New("runner.image is required when runner is declared")
	}
	seen := make(map[string]struct{}, len(list.Lines))
	for n, promise := range list.Lines {
		if err := validatePromise(list, promise); err != nil {
			return List{}, fmt.Errorf("promise %d: %w", n+1, err)
		}
		if _, exists := seen[promise.ID]; exists {
			return List{}, fmt.Errorf("promise %d: id %q appears more than once", n+1, promise.ID)
		}
		seen[promise.ID] = struct{}{}
	}
	return list, nil
}

func validateApp(app *App) error {
	if app == nil {
		return nil
	}
	if strings.TrimSpace(app.Image) == "" {
		return errors.New("app.image is required when app is declared")
	}
	if app.Port < 1 || app.Port > 65535 {
		return fmt.Errorf("app.port %d is not a TCP port", app.Port)
	}
	if !strings.HasPrefix(app.Ready.Path, "/") {
		return fmt.Errorf("app.ready.path %q must start with /", app.Ready.Path)
	}
	if app.Ready.Status < 100 || app.Ready.Status > 599 {
		return fmt.Errorf("app.ready.status %d is not an HTTP status", app.Ready.Status)
	}
	if err := validateDuration("app.ready.timeout", app.Ready.Timeout); err != nil {
		return err
	}
	for name := range app.Env {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "= ") {
			return fmt.Errorf("app.env name %q is not an environment variable name", name)
		}
	}
	return validateFixture(app.Fixture)
}

func validateFixture(fixture *Fixture) error {
	if fixture == nil {
		return nil
	}
	if !strings.HasPrefix(fixture.Dir, "/") || fixture.Dir == "/" {
		return fmt.Errorf("app.fixture.dir %q must be an absolute path inside the container, and not /", fixture.Dir)
	}
	for name := range fixture.Files {
		if !isCleanRelativePath(name) {
			return fmt.Errorf("app.fixture.files path %q is not a clean relative path", name)
		}
	}
	return nil
}

func validatePromise(list List, promise Promise) error {
	if strings.TrimSpace(promise.ID) == "" {
		return errors.New("id is required")
	}
	if strings.TrimSpace(promise.Promise) == "" {
		return errors.New("promise text is required; the plain-language half is what a person reads")
	}
	if err := validateDuration("timeout", promise.Timeout); err != nil {
		return err
	}
	if promise.Falsify == nil {
		// Deliberately legal. The run reports it as not-runnable, which is
		// never a pass, and that is how a proposed line with no command yet
		// stays visible instead of being rejected outright.
		return nil
	}
	shapes := 0
	if len(promise.Falsify.HTTP) > 0 {
		shapes++
	}
	if len(promise.Falsify.Command) > 0 {
		shapes++
	}
	if promise.Falsify.Script != nil {
		shapes++
	}
	switch shapes {
	case 0:
		return errors.New("falsify is declared but empty; omit it to record the line as not yet runnable, rather than leaving a falsifier that checks nothing")
	case 1:
	default:
		return errors.New("falsify must carry exactly one of http, command, or script")
	}
	if len(promise.Falsify.HTTP) > 0 {
		if list.App == nil {
			return errors.New("an http falsifier needs an app to drive, but the list declares no app")
		}
		for n, step := range promise.Falsify.HTTP {
			if err := validateHTTPStep(step); err != nil {
				return fmt.Errorf("http step %d: %w", n+1, err)
			}
		}
	}
	if len(promise.Falsify.Command) > 0 || promise.Falsify.Script != nil {
		if list.Runner == nil {
			return errors.New("a command or script falsifier needs runner.image, which the list does not declare")
		}
	}
	if promise.Falsify.Script != nil {
		if !strings.HasPrefix(promise.Falsify.Script.Path, "/") {
			return fmt.Errorf("script.path %q must be an absolute path inside the runner image; check 3 mounts no repository, so a driver script cannot be named by a path in the project", promise.Falsify.Script.Path)
		}
	}
	return nil
}

func validateHTTPStep(step HTTPStep) error {
	if strings.TrimSpace(step.Method) == "" {
		return errors.New("method is required")
	}
	if step.Method != strings.ToUpper(step.Method) {
		return fmt.Errorf("method %q must be upper case", step.Method)
	}
	if !strings.HasPrefix(step.Path, "/") {
		return fmt.Errorf("path %q must start with /", step.Path)
	}
	if step.Expect.Status < 100 || step.Expect.Status > 599 {
		return fmt.Errorf("expect.status %d is not an HTTP status", step.Expect.Status)
	}
	for name := range step.Headers {
		if strings.TrimSpace(name) == "" {
			return errors.New("a header name is empty")
		}
	}
	return nil
}

func validateDuration(field, value string) error {
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s %q is not a duration: %w", field, value, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s %q must be positive", field, value)
	}
	return nil
}

func isCleanRelativePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	clean := path.Clean(name)
	return clean == name && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

// The three helpers below are a deliberate second copy of the strict-JSON
// reading internal/examiner/inputs.go already does for the examiner's own
// inputs. They are not shared because the examiner's copy sits inside the
// fingerprinted agent-source tree (runtime_identity.go embeds
// internal/examiner/*.go), so moving it would make every already-built
// examiner runtime image stale for a refactor that changes no behavior. If
// that tree is ever reorganized, these are the two places to reconcile.
func requireJSONArrayField(data []byte, field string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	value, ok := fields[field]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return fmt.Errorf("%s is required and must be an array", field)
	}
	var array []json.RawMessage
	if err := json.Unmarshal(value, &array); err != nil {
		return fmt.Errorf("%s must be an array", field)
	}
	return nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("contains more than one JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var readValue func() error
	readValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, exists := keys[key]; exists {
					return fmt.Errorf("duplicate field %q", key)
				}
				keys[key] = struct{}{}
				if err := readValue(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := readValue(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected delimiter %q", delim)
		}
	}
	if err := readValue(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("contains more than one JSON value")
		}
		return err
	}
	return nil
}
