package inspector

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"sync"
)

// CheckResult is the outcome of actually running the project's check
// command.
type CheckResult struct {
	ExitCode int
	Output   string
}

// RunCheck runs command with `sh -c` from repoRoot. Output is streamed to
// stdout/stderr live and also captured for the local report. The two
// streams are written from different goroutines, but every write - to the
// capture buffer and to the caller's writers alike - is serialized on one
// lock, so passing the same writer for both stdout and stderr is safe. The
// returned error is non-nil only for infrastructure failures (e.g. sh could
// not be started) - a failing check is a normal CheckResult with a non-zero
// ExitCode, not a Go error.
func RunCheck(repoRoot, command string, stdout, stderr io.Writer) (CheckResult, error) {
	// os/exec copies stdout and stderr on separate goroutines unless the
	// two writers are the identical value, and tee never is, so the shared
	// capture buffer and the caller's writers all have to be safe for
	// concurrent writes.
	capture := &syncWriter{}

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = repoRoot
	cmd.Stdout = capture.tee(stdout)
	cmd.Stderr = capture.tee(stderr)

	// cmd.Stdout/cmd.Stderr are io.Writer values, not *os.File, so
	// os/exec owns the pipes itself: it copies through its own
	// goroutines and cmd.Run's Wait blocks until those goroutines drain
	// to EOF before returning. Using cmd.StdoutPipe/StderrPipe instead
	// would hand that draining to the caller, and calling Wait before
	// the caller has read to EOF is the classic way to truncate output,
	// since Wait closes the read end once the process exits.
	err := cmd.Run()
	if err == nil {
		return CheckResult{ExitCode: 0, Output: capture.String()}, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return CheckResult{ExitCode: exitErr.ExitCode(), Output: capture.String()}, nil
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
