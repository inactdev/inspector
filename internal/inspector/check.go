package inspector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

// CheckResult is the outcome of actually running the project's check
// command.
type CheckResult struct {
	ExitCode int
	// Signal is set when the check command was terminated by a signal
	// (e.g. the OOM killer, an external kill, inspector's own timeout)
	// rather than exiting on its own - detected two ways, since a
	// signal-killed process only sometimes looks like one. A directly
	// signaled process reports it through the OS wait status, and
	// ExitCode is -1 in that case (Go's os/exec convention, not a real
	// exit status). But `sh -c "a && b"` forks a child per command and
	// survives a killed one, exiting normally with 128+N - the shell's
	// own convention for "my child died from signal N" - so Signal can
	// also be set with ExitCode holding that 128+N value rather than
	// -1. Either way, callers must check Signal rather than assume a
	// nonzero ExitCode is the process's own verdict on the code.
	Signal string
	// TimedOut is set when RunCheck itself killed the command for
	// exceeding timeout, as distinct from some other signal (Signal is
	// also set in this case, since killing it is how the timeout is
	// enforced, but TimedOut identifies who pulled the trigger and why).
	TimedOut bool
	// KillFailed is set when the deadline fired and docker then refused
	// to stop the container, so inspector cannot claim the check command
	// is no longer running against the repo. It is additive reporting
	// only - classifyResult neither sets nor reads it, and no outcome
	// depends on it - but a caller phrasing a timeout to a human must
	// check it rather than say the check was killed when it may not have
	// been.
	KillFailed bool
	Output     string
}

// RunCheck runs command inside a fresh container built from image,
// bind-mounted to repoRoot at container.WorkspaceDir - plus, read-only,
// whatever container.ResolveGitMounts says the repo needs for git to
// work in there, and nothing else on the host - killing the container
// if it has not finished within timeout. Network access is denied
// unless network is true - see README.md for that tradeoff. Output is streamed to stdout/stderr live
// and also captured for the local report. The two streams are written
// from different goroutines, but every write - to the capture buffer and
// to the caller's writers alike - is serialized on one lock, so passing
// the same writer for both stdout and stderr is safe. The returned error
// is non-nil only for infrastructure failures (e.g. docker could not be
// started, the image could not be run at all, or the repo's git
// directory could not be made available in there) - a failing check is a
// normal CheckResult with a non-zero ExitCode, not a Go error. Callers
// are expected to have already confirmed a usable runtime via
// container.EnsureAvailable - RunCheck itself does not check again, so
// its own failures here are the rarer kind: the daemon going away
// mid-run, an image that doesn't exist, and the like. A container that
// could not be stopped at the deadline is not one of them: that stays a
// TimedOut CheckResult, with docker's own refusal reported on stderr and
// kept in Output.
func RunCheck(repoRoot, command, image string, network bool, timeout time.Duration, stdout, stderr io.Writer) (CheckResult, error) {
	// os/exec copies stdout and stderr on separate goroutines unless the
	// two writers are the identical value, and tee never is, so the shared
	// capture buffer and the caller's writers all have to be safe for
	// concurrent writes.
	capture := &syncWriter{}

	// Resolved before anything starts, so a repo whose git directory
	// cannot be reached inside the container is refused rather than
	// handed a check command whose every git call would fail. The error
	// wraps container.ErrGitDirUnavailable; Run turns that into a
	// refusal naming it.
	gitMounts, err := container.ResolveGitMounts(repoRoot)
	if err != nil {
		return CheckResult{}, err
	}
	defer gitMounts.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	run := container.New(ctx, container.Run{
		RepoRoot:       repoRoot,
		Command:        command,
		Image:          image,
		Network:        network,
		ReadOnlyMounts: gitMounts.Mounts,
	})
	defer run.Cleanup()
	run.Stdout = capture.tee(stdout)
	run.Stderr = capture.tee(stderr)

	// cmd.Stdout/cmd.Stderr are io.Writer values, not *os.File, so
	// os/exec owns the pipes itself: it copies through its own
	// goroutines and cmd.Run's Wait blocks until those goroutines drain
	// to EOF before returning. Using cmd.StdoutPipe/StderrPipe instead
	// would hand that draining to the caller, and calling Wait before
	// the caller has read to EOF is the classic way to truncate output,
	// since Wait closes the read end once the process exits.
	err = run.Run()
	if err != nil && !run.Started() {
		// docker never got as far as creating the container - a bad
		// image, an unreachable registry, the daemon going away mid-
		// call. Whatever exit code the docker CLI produced belongs to
		// docker, not to command, so this cannot become a CheckResult:
		// doing so would risk reading it as the check's own verdict.
		return CheckResult{}, fmt.Errorf("starting the container: %w", err)
	}

	// A container inspector tried and failed to stop is the one outcome
	// that must never be silent, and run.Run's error can't be trusted to
	// carry it either way: exec.Cmd.Wait drops Cancel's error whenever
	// the `docker run` client itself exited badly - exactly the case
	// where the container was left running - and surfaces it as a bare
	// Go error in the harmless race where the container had already
	// finished, which would cost this run both its timeout verdict and
	// its saved report. container.Cmd records the kill's own outcome
	// instead, so read it from there and put it in front of the person
	// running the check, in the report as well as live on stderr.
	killErr := run.KillError()
	if killErr != nil {
		fmt.Fprintf(run.Stderr, "\ninspector: could not stop the check container: %v\n"+
			"inspector: it may still be running against this repo - check `docker ps` and stop it by hand\n", killErr)
	}

	result, resultErr := classifyResult(run.Cmd, err, capture.String())
	if resultErr != nil {
		if killErr == nil {
			return CheckResult{}, resultErr
		}
		// The only way here is Wait reporting the failed cancellation
		// itself, which says nothing about the check command. Cancel
		// runs only once the deadline has fired, so this is a timeout,
		// and the block below is what states that.
		result = CheckResult{ExitCode: -1, Output: capture.String()}
	}

	// The Go documentation for CommandContext recommends judging a
	// timeout from ctx.Err() rather than from the shape of the returned
	// error, since Cancel firing doesn't guarantee any particular error
	// value once the process is reaped - including a race where the
	// container happens to exit cleanly on its own right as the deadline
	// fires, before the kill lands. Applied last and unconditionally, so
	// that race can't leave a timed-out run looking like ExitCode 0.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		result.ExitCode = -1
		if result.Signal == "" {
			result.Signal = "killed"
		}
	}
	result.KillFailed = killErr != nil
	return result, nil
}

// classifyResult turns cmd.Run's return into a CheckResult, given the
// output already captured. The returned error is reserved for genuine
// infrastructure failures - the caller's own writer failed - as opposed
// to the check command's own exit status, which never produces a Go
// error here.
func classifyResult(cmd *exec.Cmd, err error, output string) (CheckResult, error) {
	if err == nil {
		return CheckResult{ExitCode: 0, Output: output}, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return CheckResult{
			ExitCode: exitErr.ExitCode(),
			Signal:   classifySignal(exitErr.ProcessState),
			Output:   output,
		}, nil
	}

	// cmd.Run can fail without an *exec.ExitError in two situations,
	// both still leaving cmd.ProcessState populated correctly from the
	// local `docker run` client's own completed wait4:
	//
	//  - WaitDelay elapsed waiting for the docker CLI's own stdout/
	//    stderr to close after it exited (e.g. a hung daemon connection
	//    that left a pipe open) - the returned error wraps
	//    exec.ErrWaitDelay. Containerizing removed the previous, more
	//    common cause (a check command leaking a background process
	//    that outlived it while still holding the pipe): the kernel
	//    tears down a container's whole PID namespace the moment its
	//    init process exits, so nothing inside it can outlive that exit
	//    the way a host subprocess could. This branch is kept as
	//    defensive handling for the docker-CLI-level case, not because
	//    the container case is expected to hit it in practice.
	//  - The container exited (naturally, or because container.Cmd's own
	//    kill lost the race and found it already gone) right as
	//    RunCheck's deadline fired. killContainer maps that "already
	//    gone" result to os.ErrProcessDone, which is exec.Cmd.Cancel's
	//    documented way of saying Wait should keep reflecting the
	//    process's own real exit instead of treating the race as a
	//    Cancel failure - but if the local `docker run` process was
	//    still finishing its own exit bookkeeping at that exact instant,
	//    Wait can instead adopt the context's own error, surfacing as a
	//    bare context.DeadlineExceeded.
	//
	// Gated specifically to these two errors, not any non-ExitError: a
	// caller's own writer failing (e.g. stdout redirected to a full
	// disk) reaches this branch too, with ProcessState populated and a
	// misleadingly clean exit code, but surfaces as neither of these -
	// so it correctly falls through instead of being folded into a
	// false green.
	if (errors.Is(err, exec.ErrWaitDelay) || errors.Is(err, context.DeadlineExceeded)) && cmd.ProcessState != nil {
		return CheckResult{
			ExitCode: cmd.ProcessState.ExitCode(),
			Signal:   classifySignal(cmd.ProcessState),
			Output:   output,
		}, nil
	}

	return CheckResult{}, err
}

// classifySignal reports what killed the process behind ps, checking
// both of the ways a POSIX wait status can say so. A process signaled
// directly reports it through WaitStatus.Signaled() - which is how the
// local `docker run` client itself would show up, in the rare case Go's
// own WaitDelay fallback force-killed it directly. But `sh -c "a && b"`
// forks a child per command and waits on it, so a compound command's own
// killed child doesn't signal sh itself - sh instead exits normally with
// 128+N, the shell's own convention for "my child died from signal N,"
// which is what killing a timed-out compound check command, an OOM-
// killed one, or a container.Cmd.Cancel-killed one, actually looks like
// from here: the check command still runs as `sh -c command` (now inside
// the container rather than on the host), so the same convention
// applies unchanged. Returns "" when neither applies.
func classifySignal(ps *os.ProcessState) string {
	if status, ok := ps.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return status.Signal().String()
	}
	if code := ps.ExitCode(); code >= 128 {
		return syscall.Signal(code - 128).String()
	}
	return ""
}

type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// tee returns a writer that copies to dst and to w, holding w's lock for
// both, so concurrent stdout and stderr copies never write to dst at the
// same time.
func (w *syncWriter) tee(dst io.Writer) io.Writer {
	return &teeWriter{capture: w, dst: dst}
}

type teeWriter struct {
	capture *syncWriter
	dst     io.Writer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	t.capture.mu.Lock()
	defer t.capture.mu.Unlock()

	n, err := t.dst.Write(p)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	t.capture.buf.Write(p)
	return len(p), nil
}
