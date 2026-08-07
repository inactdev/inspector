package inspector

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
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
	var buf bytes.Buffer

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = repoRoot
	cmd.Stdout = io.MultiWriter(stdout, &buf)
	cmd.Stderr = io.MultiWriter(stderr, &buf)

	err := cmd.Run()
	if err == nil {
		return CheckResult{ExitCode: 0, Output: buf.String()}, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return CheckResult{ExitCode: exitErr.ExitCode(), Output: buf.String()}, nil
	}
	return CheckResult{}, err
}
