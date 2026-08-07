package inspector

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testTimeout is generous enough that no test below should ever hit it -
// tests of the timeout behavior itself use their own short value.
const testTimeout = 10 * time.Second

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func TestRunCheck_Success(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	result, err := RunCheck(dir, "echo hi", testTimeout, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Output, "hi") {
		t.Fatalf("Output = %q, want it to contain %q", result.Output, "hi")
	}
	if !strings.Contains(stdout.String(), "hi") {
		t.Fatalf("stdout was not streamed: %q", stdout.String())
	}
}

func TestRunCheck_Failure(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	result, err := RunCheck(dir, "echo boom >&2; exit 7", testTimeout, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", result.ExitCode)
	}
	if !strings.Contains(result.Output, "boom") {
		t.Fatalf("Output = %q, want it to contain %q", result.Output, "boom")
	}
}

func TestRunCheck_CapturesInterleavedStreams(t *testing.T) {
	// os/exec copies stdout and stderr on separate goroutines, so the
	// shared capture buffer has to be safe for concurrent writes. Under
	// -race this fails if that safety is dropped; without it, a losing
	// interleaving drops lines from the report.
	dir := t.TempDir()
	const lines = 200

	result, err := RunCheck(dir, fmt.Sprintf("for i in $(seq 1 %d); do echo out; echo err >&2; done", lines), testTimeout, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("check command itself failed: ExitCode = %d, output = %q", result.ExitCode, result.Output)
	}
	if got := strings.Count(result.Output, "out"); got != lines {
		t.Fatalf("captured %d stdout lines, want %d", got, lines)
	}
	if got := strings.Count(result.Output, "err"); got != lines {
		t.Fatalf("captured %d stderr lines, want %d", got, lines)
	}
}

func TestRunCheck_SameWriterForBothStreams(t *testing.T) {
	// Passing one writer for both streams is the natural way to ask for
	// combined output, and bytes.Buffer is not safe for concurrent use.
	// Under -race this fails if the two copy goroutines are allowed to
	// write to it unsynchronized.
	dir := t.TempDir()
	const lines = 200
	var combined bytes.Buffer

	result, err := RunCheck(dir, fmt.Sprintf("for i in $(seq 1 %d); do echo out; echo err >&2; done", lines), testTimeout, &combined, &combined)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("check command itself failed: ExitCode = %d, output = %q", result.ExitCode, result.Output)
	}
	if got := strings.Count(combined.String(), "out"); got != lines {
		t.Fatalf("streamed %d stdout lines, want %d", got, lines)
	}
	if got := strings.Count(combined.String(), "err"); got != lines {
		t.Fatalf("streamed %d stderr lines, want %d", got, lines)
	}
}

func TestRunCheck_CapturesFullOutputAcrossProcessExit(t *testing.T) {
	// Regression guard for the classic exec.Cmd pitfall: os/exec's Wait
	// closes the child's stdout/stderr pipes once the process exits, so
	// code that reads those pipes manually and calls Wait too early
	// truncates output. RunCheck avoids the pitfall by construction -
	// cmd.Stdout/cmd.Stderr are plain io.Writer values, not *os.File, so
	// os/exec runs its own copy goroutines and Wait (called inside
	// cmd.Run) blocks until they drain to EOF before returning. This
	// writes far more than one pipe buffer right up to process exit, so
	// a premature Wait would show up as a short read.
	dir := t.TempDir()
	const size = 500_000

	result, err := RunCheck(dir, fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'a'", size), testTimeout, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("check command itself failed: ExitCode = %d, output = %q", result.ExitCode, truncate(result.Output, 200))
	}
	if len(result.Output) != size {
		t.Fatalf("captured %d bytes, want %d - output was truncated", len(result.Output), size)
	}
}

func TestRunCheck_SignalKilled(t *testing.T) {
	dir := t.TempDir()

	result, err := RunCheck(dir, "kill -9 $$", testTimeout, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Signal == "" {
		t.Fatalf("expected Signal to be set, got CheckResult %+v", result)
	}
	if !strings.Contains(result.Signal, "killed") {
		t.Fatalf("Signal = %q, want it to describe SIGKILL", result.Signal)
	}
}

func TestRunCheck_CompoundCommandSurvivingKilledChild(t *testing.T) {
	// `sh -c "a && b"` forks a for each of a and b and waits on them, so
	// when a is a separate process that gets killed by a signal - not
	// `kill -9 $$`, which kills the interpreter running it directly -
	// sh itself is never signaled. It sees its child's wait status and
	// exits normally with 128+N, the shell's own convention for "my
	// child died from signal N." Verified directly: `sh -c "kill -9 $$"
	// && true` from Go's os/exec reports ExitCode 137, Signaled false.
	// README's own example check command has exactly this shape (`npm
	// test && npm run lint`), so this is the common case, not an edge
	// case.
	dir := t.TempDir()

	result, err := RunCheck(dir, `sh -c 'kill -9 $$' && true`, testTimeout, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 137 {
		t.Fatalf("ExitCode = %d, want 137 (sh's own 128+9 convention)", result.ExitCode)
	}
	if result.Signal == "" {
		t.Fatalf("expected Signal to be set from the 128+N exit code even though sh itself wasn't signaled, got CheckResult %+v", result)
	}
	if !strings.Contains(result.Signal, "killed") {
		t.Fatalf("Signal = %q, want it to describe SIGKILL", result.Signal)
	}
}

func TestRunCheck_Timeout(t *testing.T) {
	dir := t.TempDir()

	start := time.Now()
	result, err := RunCheck(dir, "sleep 30", 200*time.Millisecond, io.Discard, io.Discard)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("expected TimedOut, got CheckResult %+v", result)
	}
	if result.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1 (the documented convention for a signaled process) - a timed-out run must never persist an exit code that reads as a pass", result.ExitCode)
	}
	if result.Signal == "" {
		t.Fatalf("expected Signal to be set on a timed-out result, got CheckResult %+v", result)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("RunCheck took %s to return after a 200ms timeout - the kill isn't taking effect promptly", elapsed)
	}
}

func TestRunCheck_TimeoutKillsChildProcesses(t *testing.T) {
	// The direct child of RunCheck is always `sh`; a compound command
	// (README's own example is `npm test && npm run lint`) forks its
	// own children under that shell. Killing sh alone would leave a
	// hung child running past the timeout it was supposed to enforce.
	// Proves it by having a background grandchild announce that it
	// woke up naturally, into a temp file, well after the timeout - if
	// the process-group kill works, that file stays empty.
	dir := t.TempDir()
	marker := filepath.Join(dir, "survived")

	_, err := RunCheck(dir, fmt.Sprintf("( sleep 1 && echo alive > %s ) & wait", marker), 100*time.Millisecond, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Give a leaked background process the time it would have needed
	// to write the marker, then confirm it never did.
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("background child survived the timeout kill and wrote %s", marker)
	}
}

func TestRunCheck_LeakedSubprocessDoesNotDiscardOutput(t *testing.T) {
	// A check command can finish successfully while leaving a
	// subprocess behind that still holds stdout/stderr open (e.g.
	// `npm test && (npm run dev &)`). Go's own wait4 on the primary
	// process already completed by then, but cmd.Run doesn't return
	// nil - it returns an error wrapping exec.ErrWaitDelay once
	// WaitDelay (5s) elapses waiting for the pipes to close. Confirms
	// RunCheck uses cmd.ProcessState instead of discarding the result
	// as an infrastructure failure. Genuinely takes just over 5s: this
	// is exercising the real WaitDelay, not a shortened stand-in for it.
	if testing.Short() {
		t.Skip("waits out a real 5s WaitDelay; skipped in -short")
	}
	dir := t.TempDir()

	result, err := RunCheck(dir, "echo hello; (sleep 30 &)", testTimeout, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v - a leaked subprocess should not turn a finished check into an infrastructure failure", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 - the primary command succeeded", result.ExitCode)
	}
	if !strings.Contains(result.Output, "hello") {
		t.Fatalf("Output = %q, want it to contain the primary command's output, not be discarded", result.Output)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("simulated write failure")
}

func TestRunCheck_CallerWriterFailureIsNotSwallowed(t *testing.T) {
	// A caller's own writer can fail for reasons that have nothing to
	// do with the check command itself (stdout redirected to a full
	// disk, a library caller whose writer errors) - cmd.Run returns
	// that error directly, unwrapped, with ProcessState populated and a
	// misleadingly clean exit code, which looks identical in shape to
	// the WaitDelay/leaked-subprocess case RunCheck deliberately
	// tolerates. The two must not be conflated: this one is a real
	// infrastructure failure, and folding it into a false green would
	// silently discard however much output never made it to the
	// caller.
	dir := t.TempDir()

	_, err := RunCheck(dir, "echo hi", testTimeout, failingWriter{}, io.Discard)
	if err == nil {
		t.Fatal("expected an error when the caller's own writer fails, not a swallowed success")
	}
}

func TestRunCheck_RunsFromRepoRoot(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	result, err := RunCheck(dir, "pwd -P", testTimeout, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("check command itself failed: ExitCode = %d, output = %q", result.ExitCode, result.Output)
	}
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving symlinks for %s: %v", dir, err)
	}
	if strings.TrimSpace(result.Output) != wantDir {
		t.Fatalf("check did not run from %q: got %q", wantDir, result.Output)
	}
}
