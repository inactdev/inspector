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
// stdout/stderr live and also captured for the local report. The returned
// error is non-nil only for infrastructure failures (e.g. sh could not be
// started) - a failing check is a normal CheckResult with a non-zero
// ExitCode, not a Go error.
func RunCheck(repoRoot, command string, stdout, stderr io.Writer) (CheckResult, error) {
	// os/exec copies stdout and stderr on separate goroutines unless the
	// two writers are the identical value, so the shared capture buffer
	// has to be safe for concurrent writes.
	capture := &syncWriter{}

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = repoRoot
	cmd.Stdout = io.MultiWriter(stdout, capture)
	cmd.Stderr = io.MultiWriter(stderr, capture)

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

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
