package inspector

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

// testTimeout is generous enough that no test below should ever hit it -
// tests of the timeout behavior itself use their own short value.
const testTimeout = 30 * time.Second

// testImage is a small, fast-to-pull image with a POSIX shell, used by
// every RunCheck test below - none of them depend on anything beyond
// that.
const testImage = "alpine"

// gitTestImage is a small official library image carrying git and a
// shell, for the few tests whose check command actually uses git -
// testImage carries no git at all.
const gitTestImage = "buildpack-deps:bookworm-scm"

// requireDocker skips a test that genuinely needs a live container
// runtime, rather than failing on a machine without one - the same
// tolerance LoadConfig's own permission-bit test already extends to a
// root-run suite.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := container.EnsureAvailable(); err != nil {
		t.Skipf("no usable container runtime, skipping: %v", err)
	}
}

// requireInternet skips a test that needs to actually reach the outside
// world, the same way requireDocker skips one that needs a runtime: a
// machine with Docker but no outbound access, or an example.com blip,
// shouldn't fail the suite.
func requireInternet(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "example.com:80", 5*time.Second)
	if err != nil {
		t.Skipf("no outbound internet access, skipping: %v", err)
	}
	conn.Close()
}

// failingDockerKill puts a docker shim first on PATH that hands every
// subcommand to the real docker except the timeout's `kill` and fallback
// `rm --force`, which it fails the way docker itself does for a container
// that is no longer running. That makes the failed-cleanup path
// reproducible instead of dependent on winning a race with --rm's
// auto-remove window, and the names it was asked to kill are what the
// test cleanup needs to remove the containers that consequently outlived
// their run.
func failingDockerKill(t *testing.T) {
	t.Helper()
	realDocker, err := exec.LookPath("docker")
	if err != nil {
		t.Skipf("no docker on PATH, skipping: %v", err)
	}

	dir := t.TempDir()
	names := filepath.Join(dir, "kill-attempts")
	shim := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "kill" ]; then
	printf '%%s\n' "$2" >> %q
	echo "Error response from daemon: Cannot kill container: $2: Container $2 is not running" >&2
	exit 1
fi
if [ "$1" = "rm" ] && [ "$2" = "--force" ]; then
	echo "Error response from daemon: Cannot remove container: $3: Container $3 is not running" >&2
	exit 1
fi
exec %q "$@"
`, names, realDocker)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(shim), 0o755); err != nil {
		t.Fatalf("writing the docker shim: %v", err)
	}

	t.Cleanup(func() {
		attempted, err := os.ReadFile(names)
		if err != nil {
			return
		}
		for _, name := range strings.Fields(string(attempted)) {
			_ = exec.Command(realDocker, "rm", "-f", name).Run()
		}
	})
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func TestRunCheck_Success(t *testing.T) {
	requireDocker(t)
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	result, err := RunCheck(dir, "echo hi", testImage, false, testTimeout, &stdout, &stderr)
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
	requireDocker(t)
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	result, err := RunCheck(dir, "echo boom >&2; exit 7", testImage, false, testTimeout, &stdout, &stderr)
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
	requireDocker(t)
	dir := t.TempDir()
	const lines = 200

	result, err := RunCheck(dir, fmt.Sprintf("for i in $(seq 1 %d); do echo out; echo err >&2; done", lines), testImage, false, testTimeout, io.Discard, io.Discard)
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
	requireDocker(t)
	dir := t.TempDir()
	const lines = 200
	var combined bytes.Buffer

	result, err := RunCheck(dir, fmt.Sprintf("for i in $(seq 1 %d); do echo out; echo err >&2; done", lines), testImage, false, testTimeout, &combined, &combined)
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
	requireDocker(t)
	dir := t.TempDir()
	const size = 500_000

	result, err := RunCheck(dir, fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'a'", size), testImage, false, testTimeout, io.Discard, io.Discard)
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

func TestClassifySignal_DirectlySignaledProcess(t *testing.T) {
	// classifySignal's WaitStatus.Signaled() branch is for the local
	// `docker run` client process itself being signaled directly -
	// which container.Cmd's own timeout handling never does (it stops
	// the container via `docker kill`, not the local process; see
	// container.New), so it isn't reachable by driving RunCheck end to
	// end. Constructs a real signaled *os.ProcessState the direct way -
	// a plain host process, nothing to do with containers - purely to
	// get valid input for the branch itself.
	cmd := exec.Command("sh", "-c", "kill -9 $$")
	err := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run() = %v, want an *exec.ExitError from a signaled process", err)
	}
	got := classifySignal(exitErr.ProcessState)
	if !strings.Contains(got, "killed") {
		t.Fatalf("classifySignal(directly signaled) = %q, want it to describe SIGKILL", got)
	}
}

func TestRunCheck_CompoundCommandSurvivingKilledChild(t *testing.T) {
	// `sh -c "a && b"` forks a child for each of a and b and waits on
	// them, so when a is a separate process that gets killed by a signal
	// - not `kill -9 $$`, which kills the interpreter running it
	// directly - sh itself is never signaled. It sees its child's wait
	// status and exits normally with 128+N, the shell's own convention
	// for "my child died from signal N." README's own example check
	// command has exactly this shape (`npm test && npm run lint`), so
	// this is the common case, not an edge case - and it plays out
	// identically whether sh is running on the host or, as here, inside
	// the container.
	requireDocker(t)
	dir := t.TempDir()

	result, err := RunCheck(dir, `sh -c 'kill -9 $$' && true`, testImage, false, testTimeout, io.Discard, io.Discard)
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
	requireDocker(t)
	dir := t.TempDir()

	start := time.Now()
	result, err := RunCheck(dir, "sleep 30", testImage, false, 300*time.Millisecond, io.Discard, io.Discard)
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
	if result.KillFailed {
		t.Fatalf("KillFailed = true on a timeout whose kill docker accepted, got CheckResult %+v", result)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("RunCheck took %s to return after a 300ms timeout - the kill isn't taking effect promptly", elapsed)
	}
}

func TestRunCheck_TimeoutKillsChildProcesses(t *testing.T) {
	// check.go's old direct execution killed the process group led by
	// `sh` when the timeout fired, because `sh -c "a && b"` forks its
	// own children and killing sh alone would leave a hung compound
	// command's children running past the deadline. Containerizing
	// replaced that with killing the container itself
	// (internal/container): the check command's `sh` now runs as the
	// container's own init process, and the kernel tears down its whole
	// PID namespace - including any children it forked - the instant
	// that process is killed. Proves it the same way the host version
	// did: a background grandchild announces that it woke up naturally,
	// into a file inside the bind-mounted repo, well after the timeout -
	// if the kill works, that file stays empty.
	requireDocker(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "survived")

	_, err := RunCheck(dir, "( sleep 3 && echo alive > survived ) & wait", testImage, false, 300*time.Millisecond, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Give a leaked background process the time it would have needed
	// to write the marker, then confirm it never did.
	time.Sleep(3200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("background child survived the timeout kill and wrote %s", marker)
	}
}

func TestRunCheck_RunsFromWorkspaceDir(t *testing.T) {
	// The container mounts the repo at a fixed internal path
	// (container.WorkspaceDir), not wherever it happens to live on the
	// host - the whole point is that nothing outside that mount is
	// reachable, including the host's own directory layout.
	requireDocker(t)
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	result, err := RunCheck(dir, "pwd", testImage, false, testTimeout, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("check command itself failed: ExitCode = %d, output = %q", result.ExitCode, result.Output)
	}
	if strings.TrimSpace(result.Output) != container.WorkspaceDir {
		t.Fatalf("check did not run from %q: got %q", container.WorkspaceDir, result.Output)
	}
}

func TestRunCheck_BindMountsOnlyTheRepo(t *testing.T) {
	// The acceptance bar for issue #13: the container can reach the repo
	// and nothing else on the host. Writes a marker outside dir, then
	// confirms a check command inside the container can't see it -
	// proof the mount is scoped to dir rather than, say, the whole host
	// filesystem via some broader default.
	requireDocker(t)
	outside := t.TempDir()
	dir := t.TempDir()
	writeFiles(t, outside, map[string]string{"secret.txt": "should not be visible"})

	result, err := RunCheck(dir, fmt.Sprintf("test -e %s && echo LEAKED || echo contained", outside), testImage, false, testTimeout, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result.Output, "LEAKED") {
		t.Fatalf("check command could see a host path outside the bind-mounted repo: %q", result.Output)
	}
	if !strings.Contains(result.Output, "contained") {
		t.Fatalf("Output = %q, want it to report the outside path as absent", result.Output)
	}
}

func TestRunCheck_NoNetworkByDefault(t *testing.T) {
	requireDocker(t)
	dir := t.TempDir()

	result, err := RunCheck(dir, "wget -q -T 3 -O /dev/null http://example.com && echo REACHED || echo blocked", testImage, false, testTimeout, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result.Output, "REACHED") {
		t.Fatal("check command reached the network with Network left at its false default")
	}
	if !strings.Contains(result.Output, "blocked") {
		t.Fatalf("Output = %q, want it to report the network as unreachable", result.Output)
	}
}

func TestRunCheck_NetworkOptIn(t *testing.T) {
	requireDocker(t)
	requireInternet(t)
	dir := t.TempDir()

	result, err := RunCheck(dir, "wget -q -T 5 -O /dev/null http://example.com && echo REACHED || echo blocked", testImage, true, testTimeout, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Output, "REACHED") {
		t.Fatalf("check command could not reach the network with Network: true - Output = %q (this test needs outbound internet access to pass)", result.Output)
	}
}

func TestRunCheck_UnstoppableContainerIsReported(t *testing.T) {
	// A timeout whose kill docker refused is the one case where a
	// container can outlive its deadline against the repo, so it has to
	// be visible - and still a timeout verdict, not an infrastructure
	// error that would throw away the run's report.
	requireDocker(t)
	failingDockerKill(t)
	dir := t.TempDir()
	var stderr bytes.Buffer

	result, err := RunCheck(dir, "sleep 30", testImage, false, 500*time.Millisecond, io.Discard, &stderr)
	if err != nil {
		t.Fatalf("a refused kill must stay a verdict, not become an infrastructure error: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("TimedOut = false, want true - result = %+v", result)
	}
	if !result.KillFailed {
		t.Fatalf("KillFailed = false after docker refused the kill - callers phrasing this to a human would claim the check was stopped: %+v", result)
	}
	if result.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1 - `docker kill`'s own exit status is not the check command's", result.ExitCode)
	}
	if !strings.Contains(result.Output, "is not running") {
		t.Fatalf("Output = %q, want docker's own refusal folded into the report", truncate(result.Output, 400))
	}
	if !strings.Contains(stderr.String(), "is not running") {
		t.Fatalf("stderr = %q, want the refused kill reported live", truncate(stderr.String(), 400))
	}
}

func TestRunCheck_UnusableImageIsInfrastructureFailure(t *testing.T) {
	// A bad image is docker's own failure, not a verdict on the check
	// command - it must come back as a Go error (an infrastructure
	// failure, per RunCheck's own doc comment), never as a CheckResult
	// whose ExitCode happens to be whatever the docker CLI produced.
	requireDocker(t)
	dir := t.TempDir()

	_, err := RunCheck(dir, "true", "inspector-test-image-does-not-exist-xyz", false, testTimeout, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected an error for an image that was never run, not a swallowed CheckResult")
	}
}
