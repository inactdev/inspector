package alwaystrue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

// DefaultTimeout bounds the whole run: bringing the app up, driving every
// line, and tearing it down.
const DefaultTimeout = 15 * time.Minute

// maxEvidenceBytes caps what one line's evidence keeps. The record is for a
// person to read on the way to acting, not a transcript.
const maxEvidenceBytes = 2000

// maxResponseBytes caps how much of a response body an http falsifier reads
// before deciding its expectation.
const maxResponseBytes = 1 << 20

// Options configures one check-3 run.
type Options struct {
	// ListPath is the regression list to drive. Empty means DefaultListName
	// in the current directory.
	ListPath string
	// Limits bounds the app container and every falsifier container.
	Limits  container.Limits
	Timeout time.Duration
	Stdout  io.Writer
	Stderr  io.Writer
}

// Run drives every line of the list and returns the record-derived verdict.
//
// It mounts no repository. That is the whole of how read-only is guaranteed:
// there is no handle on the commit to be read-only about, the same boundary
// the examiner has. Nothing about the judged project is written, and nothing
// has to be proved about that afterwards.
//
// Every line is attempted even after one breaks, because the verdict is one
// aggregate naming every broken promise rather than the first one found.
func Run(ctx context.Context, opts Options) (Result, error) {
	listPath := opts.ListPath
	if strings.TrimSpace(listPath) == "" {
		listPath = DefaultListName
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	list, err := Load(listPath)
	if err != nil {
		return RefusedResult(listPath, err.Error()), nil
	}
	if err := container.EnsureAvailableContext(runCtx); err != nil {
		return RefusedResult(listPath, err.Error()), nil
	}
	user, err := container.ResolveUser(runCtx)
	if err != nil {
		return RefusedResult(listPath, fmt.Sprintf("no usable container runtime: %v", err)), nil
	}

	var app *runningApp
	infrastructure := ""
	if list.App != nil {
		started, teardown, startErr := startApp(runCtx, list.App, user, opts.Limits)
		defer teardown()
		if startErr != nil {
			infrastructure = startErr.Error()
		} else {
			app = started
			report(opts.Stdout, "app up: %s at %s (fixture data is this run's own and goes away with it)\n", list.App.Image, started.HostURL)
		}
	}

	promises := make([]PromiseResult, 0, len(list.Lines))
	for _, promise := range list.Lines {
		result := PromiseResult{
			ID:        promise.ID,
			Promise:   promise.Promise,
			Confirmed: promise.Confirmed,
			Shape:     promise.Shape(),
		}
		drive(runCtx, list, promise, app, user, opts.Limits, &result)
		report(opts.Stdout, "%s: %s - %s\n", result.Outcome, promise.ID, result.Reason)
		promises = append(promises, result)
	}
	return DeriveResult(listPath, app != nil, infrastructure, promises), nil
}

// drive runs one line's falsifier and fills in its outcome. A line with no
// falsifier is not-runnable, which is never a pass: the ruling's whole point
// is that "all of them must still pass" cannot be claimed for a line nothing
// can disprove.
func drive(ctx context.Context, list List, promise Promise, app *runningApp, user string, limits container.Limits, result *PromiseResult) {
	if promise.Falsify == nil {
		result.Outcome = NotRunnable
		result.Reason = "no falsifier is declared, so nothing could be run; a promise with no command, script, or HTTP call is never reported as passing"
		return
	}
	if err := ctx.Err(); err != nil {
		result.Outcome = NotRun
		result.Reason = fmt.Sprintf("the run ended before this line was driven: %v", err)
		return
	}
	lineCtx, cancel := context.WithTimeout(ctx, promise.PromiseTimeout())
	defer cancel()

	switch result.Shape {
	case "http":
		if app == nil {
			result.Outcome = NotRun
			result.Reason = "the app it drives never came up, so this line was not attempted"
			return
		}
		driveHTTP(lineCtx, promise, app, result)
	case "command", "script":
		if list.App != nil && app == nil {
			result.Outcome = NotRun
			result.Reason = "the app it drives never came up, so this line was not attempted"
			return
		}
		driveCommand(lineCtx, list, promise, app, user, limits, result)
	default:
		result.Outcome = NotRunnable
		result.Reason = "its falsifier shape is not one this inspector can run"
	}
}

// driveHTTP performs the exchange itself from the host, against the ephemeral
// loopback port Docker published for the app. Inspector makes the request and
// compares the response, so the deterministic shape needs no shell and no
// extra image, and what was observed is recorded rather than summarized by
// something else.
func driveHTTP(ctx context.Context, promise Promise, app *runningApp, result *PromiseResult) {
	var evidence []string
	for n, step := range promise.Falsify.HTTP {
		status, body, err := doRequest(ctx, app.HostURL, step)
		if err != nil {
			// Not a broken promise: a request that never reached the app says
			// nothing about the product. It is reported as not checked, which
			// is an error state rather than a red one.
			result.Outcome = NotRun
			result.Reason = fmt.Sprintf("step %d (%s %s) could not be delivered: %v", n+1, step.Method, step.Path, err)
			result.Evidence = truncate(strings.Join(evidence, "\n"), maxEvidenceBytes)
			return
		}
		evidence = append(evidence, fmt.Sprintf("%s %s -> %d %s", step.Method, step.Path, status, truncate(oneLine(body), 300)))
		if failure := checkExpectation(step, status, body); failure != "" {
			result.Outcome = Broken
			result.Reason = fmt.Sprintf("step %d (%s %s) %s", n+1, step.Method, step.Path, failure)
			result.Evidence = truncate(strings.Join(evidence, "\n"), maxEvidenceBytes)
			return
		}
	}
	result.Outcome = Held
	result.Reason = fmt.Sprintf("%d HTTP step(s) answered as promised", len(promise.Falsify.HTTP))
	result.Evidence = truncate(strings.Join(evidence, "\n"), maxEvidenceBytes)
}

func checkExpectation(step HTTPStep, status int, body string) string {
	if status != step.Expect.Status {
		return fmt.Sprintf("answered %d, not the promised %d", status, step.Expect.Status)
	}
	for _, want := range step.Expect.BodyContains {
		if !strings.Contains(body, want) {
			return fmt.Sprintf("answered %d as promised, but its body does not contain %q", status, want)
		}
	}
	for _, unwanted := range step.Expect.BodyNotContains {
		if strings.Contains(body, unwanted) {
			return fmt.Sprintf("answered %d as promised, but its body still contains %q", status, unwanted)
		}
	}
	return ""
}

// httpClient follows no redirects. A redirect is not the promised status, so
// following one would decide the expectation against a different resource -
// and off-origin, a different server entirely.
var httpClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func doRequest(ctx context.Context, baseURL string, step HTTPStep) (int, string, error) {
	var body io.Reader
	if step.Body != "" {
		body = strings.NewReader(step.Body)
	}
	req, err := http.NewRequestWithContext(ctx, step.Method, baseURL+step.Path, body)
	if err != nil {
		return 0, "", err
	}
	for _, name := range sortedKeys(step.Headers) {
		req.Header.Set(name, step.Headers[name])
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, string(content), err
	}
	return resp.StatusCode, string(content), nil
}

// driveCommand runs a console command or driver script in its own single-use
// container from the runner image. It joins the app container's network stack
// with `--network container:<app>`, which is how it reaches the app on
// 127.0.0.1 without a second published port; with no app declared, it gets no
// network at all. The command is passed as argv rather than through a shell,
// so what ran is exactly what the list says.
func driveCommand(ctx context.Context, list List, promise Promise, app *runningApp, user string, limits container.Limits, result *PromiseResult) {
	if list.Runner == nil {
		// Parse refuses this combination, so reaching it means a List was
		// built by something other than Parse. Not-runnable rather than a
		// panic, and never a pass.
		result.Outcome = NotRunnable
		result.Reason = "it needs a runner image to run in and the list declares none"
		return
	}
	args := isolationArgs(user, limits)
	if app != nil {
		args = append(args, "--network", "container:"+app.Name, "--env", "APP_URL="+app.ContainerURL)
	} else {
		args = append(args, "--network", "none")
	}
	args = append(args, list.Runner.Image)
	command := promise.Falsify.Command
	if promise.Falsify.Script != nil {
		command = append([]string{promise.Falsify.Script.Path}, promise.Falsify.Script.Args...)
	}
	args = append(args, command...)

	cmd := container.NewCommand(ctx, "inspector-always-line-", args...)
	defer cmd.Cleanup()
	// One buffer for both streams: os/exec gives a command whose Stdout and
	// Stderr are the same writer a single pipe, so this is the interleaved
	// output rather than a concurrent write to one buffer.
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	result.Evidence = truncate(strings.TrimSpace(output.String()), maxEvidenceBytes)

	if killErr := cmd.KillError(); killErr != nil {
		result.Outcome = NotRun
		result.Reason = fmt.Sprintf("its falsifier container could not be stopped and may still be running: %v", killErr)
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Outcome = NotRun
		result.Reason = fmt.Sprintf("its falsifier did not finish within %s, so the promise was not decided either way", promise.PromiseTimeout())
		return
	}
	if runErr == nil {
		result.Outcome = Held
		result.Reason = describeCommand(command) + " exited 0"
		return
	}
	if !cmd.Started() {
		// Docker itself never got the container off the ground, so the
		// falsifier never ran. That is infrastructure, not a broken promise.
		result.Outcome = NotRun
		result.Reason = fmt.Sprintf("its falsifier container could not be started from %s, so nothing was run: %v", list.Runner.Image, runErr)
		return
	}
	result.Outcome = Broken
	result.Reason = fmt.Sprintf("%s falsified it: %v", describeCommand(command), runErr)
}

func describeCommand(command []string) string {
	if len(command) == 0 {
		return "its falsifier"
	}
	return truncate(strings.Join(command, " "), 200)
}

func report(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format, args...)
}

// truncate caps recorded evidence. The cut is made valid again rather than
// left mid-rune, so a truncated response body stays readable text.
func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return strings.ToValidUTF8(text[:max], "") + "... (truncated)"
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
