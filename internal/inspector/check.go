package inspector

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
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
	Output   string
}

// RunCheck runs command with `sh -c` from repoRoot, killing it if it has
// not finished within timeout. Output is streamed to stdout/stderr live
// and also captured for the local report. The two streams are written
// from different goroutines, but every write - to the capture buffer and
// to the caller's writers alike - is serialized on one lock, so passing
// the same writer for both stdout and stderr is safe. The returned error
// is non-nil only for infrastructure failures (e.g. sh could not be
// started) - a failing check is a normal CheckResult with a non-zero
// ExitCode, not a Go error.
func RunCheck(repoRoot, command string, timeout time.Duration, stdout, stderr io.Writer) (CheckResult, error) {
	// os/exec copies stdout and stderr on separate goroutines unless the
	// two writers are the identical value, and tee never is, so the shared
	// capture buffer and the caller's writers all have to be safe for
	// concurrent writes.
	capture := &syncWriter{}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = repoRoot
	cmd.Stdout = capture.tee(stdout)
	cmd.Stderr = capture.tee(stderr)

	// sh -c "a && b" forks a child for each command and waits on it, so
	// killing sh alone leaves a running compound command's own children
	// behind. Setpgid puts sh in a new process group with its own PID as
	// the group's, so every descendant it forks inherits membership in
	// that group unless it explicitly leaves; Cancel then kills the
	// whole group at once via the negative PID, instead of os/exec's
	// default of killing only the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 5 * time.Second

	// cmd.Stdout/cmd.Stderr are io.Writer values, not *os.File, so
	// os/exec owns the pipes itself: it copies through its own
	// goroutines and cmd.Run's Wait blocks until those goroutines drain
	// to EOF before returning. Using cmd.StdoutPipe/StderrPipe instead
	// would hand that draining to the caller, and calling Wait before
	// the caller has read to EOF is the classic way to truncate output,
	// since Wait closes the read end once the process exits.
	err := cmd.Run()
	result, resultErr := classifyResult(cmd, err, capture.String())
	if resultErr != nil {
		return CheckResult{}, resultErr
	}

	// The Go documentation for CommandContext recommends judging a
	// timeout from ctx.Err() rather than from the shape of the returned
	// error, since Cancel firing doesn't guarantee any particular error
	// value once the process is reaped - including a race where the
	// command happens to exit cleanly on its own right as the deadline
	// fires, before SIGKILL lands. Applied last and unconditionally, so
	// that race can't leave a timed-out run looking like ExitCode 0.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		result.ExitCode = -1
		if result.Signal == "" {
			result.Signal = "killed"
		}
	}
	return result, nil
}

// classifyResult turns cmd.Run's return into a CheckResult, given the
// output already captured. The returned error is reserved for genuine
// infrastructure failures - sh could not be started, or a caller's own
// writer failed - as opposed to the check command's own exit status,
// which never produces a Go error here.
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
	// process's own completed wait4:
	//
	//  - WaitDelay elapsed waiting for a leaked subprocess to release
	//    stdout/stderr (e.g. `npm test && (npm run dev &)`) - the
	//    returned error wraps exec.ErrWaitDelay.
	//  - The check process exited right as inspector's own deadline
	//    fired. If it was already fully reaped by the time Cancel's
	//    kill ran, killProcessGroup maps that to os.ErrProcessDone and
	//    this whole function is unreachable for it - Wait keeps
	//    reflecting the real exit and err is nil or an *exec.ExitError.
	//    But if it was still a zombie (exited, not yet reaped) when the
	//    kill ran, kill(-pid, SIGKILL) silently no-ops - a zombie can't
	//    act on any signal, so it "succeeds" - and Cancel returns nil
	//    rather than os.ErrProcessDone. Per exec.Cmd.Cancel's own
	//    documented contract, that makes Wait adopt the context's own
	//    error instead of the process's, so the returned error is a
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
// directly reports it through WaitStatus.Signaled(). But `sh -c "a &&
// b"` forks a child per command and waits on it, so a compound
// command's own killed child doesn't signal sh itself - sh instead
// exits normally with 128+N, the shell's own convention for "my child
// died from signal N," which is what killing a timed-out compound
// check command, or an OOM-killed one, actually looks like from here.
// Returns "" when neither applies.
func classifySignal(ps *os.ProcessState) string {
	if status, ok := ps.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return status.Signal().String()
	}
	if code := ps.ExitCode(); code >= 128 {
		return syscall.Signal(code - 128).String()
	}
	return ""
}

// killProcessGroup SIGKILLs the process group led by pid, used as
// exec.Cmd.Cancel. If the group is already gone - the check process
// exited on its own right as the deadline fired, before this ran - the
// kill fails with ESRCH; mapping that to os.ErrProcessDone rather than
// returning it raw is exec.Cmd.Cancel's documented way of saying "no
// error, nothing to do." Without that mapping, a raw ESRCH tells Wait
// this race is a Cancel failure, and Wait can then return an opaque
// error unrelated to anything actually wrong, for a timeout inspector
// itself triggered - instead of continuing to reflect whatever the
// process's own exit status turned out to be.
func killProcessGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
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
