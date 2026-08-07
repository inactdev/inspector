package inspector

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	// rather than exiting on its own. ExitCode is -1 in that case - Go's
	// os/exec convention, not a real process exit status - so callers
	// must check Signal rather than treating a -1 or otherwise nonzero
	// ExitCode as the process's own verdict on the code.
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
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	// cmd.Stdout/cmd.Stderr are io.Writer values, not *os.File, so
	// os/exec owns the pipes itself: it copies through its own
	// goroutines and cmd.Run's Wait blocks until those goroutines drain
	// to EOF before returning. Using cmd.StdoutPipe/StderrPipe instead
	// would hand that draining to the caller, and calling Wait before
	// the caller has read to EOF is the classic way to truncate output,
	// since Wait closes the read end once the process exits.
	err := cmd.Run()

	// The Go documentation for CommandContext recommends judging a
	// timeout from ctx.Err() rather than from the shape of the returned
	// error, since Cancel firing doesn't guarantee any particular error
	// value once the process is reaped.
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return CheckResult{TimedOut: true, Signal: "killed", Output: capture.String()}, nil
	}

	if err == nil {
		return CheckResult{ExitCode: 0, Output: capture.String()}, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result := CheckResult{ExitCode: exitErr.ExitCode(), Output: capture.String()}
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.Signal = status.Signal().String()
		}
		return result, nil
	}
	return CheckResult{}, err
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
