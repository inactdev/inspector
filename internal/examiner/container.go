package examiner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

const (
	DefaultImage   = "alpine:3.20"
	DefaultNetwork = "bridge"
	DefaultTimeout = 10 * time.Minute
)

// RunOptions configures the container that holds the examiner agent. It has no
// repository path by design: app source cannot be mounted into this container.
type RunOptions struct {
	Inputs     Inputs
	AppURL     string
	Model      string
	APIBaseURL string
	Image      string
	Network    string
	Timeout    time.Duration
	Executable string
	Stdout     io.Writer
	Stderr     io.Writer
}

// Run starts the sealed agent and returns its verdict. It writes only to
// temporary directories owned by Inspector, never to the project being judged.
func Run(ctx context.Context, opts RunOptions) (Result, error) {
	prepared, err := PrepareInputs(opts.Inputs)
	if err != nil {
		return Result{Kind: Refused, Message: err.Error()}, nil
	}
	defer prepared.Cleanup()
	if _, err := validAppURL(opts.AppURL); err != nil {
		return Result{Kind: Refused, Message: err.Error()}, nil
	}
	if strings.TrimSpace(opts.Model) == "" {
		return Result{Kind: Refused, Message: "no model was configured"}, nil
	}
	if _, ok := os.LookupEnv(AnthropicAPIKeyEnvVar); !ok {
		return Result{Kind: Refused, Message: fmt.Sprintf("%s is not set", AnthropicAPIKeyEnvVar)}, nil
	}
	if err := container.EnsureAvailable(); err != nil {
		return Result{Kind: Refused, Message: fmt.Sprintf("no usable container runtime: %v", err)}, nil
	}

	executable := opts.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return Result{}, fmt.Errorf("locating inspector executable: %w", err)
		}
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return Result{}, fmt.Errorf("resolving inspector executable: %w", err)
	}
	if info, err := os.Stat(executable); err != nil || info.IsDir() {
		if err == nil {
			err = errors.New("is a directory")
		}
		return Result{Kind: Refused, Message: fmt.Sprintf("examiner executable %q: %v", executable, err)}, nil
	}

	outputDir, err := os.MkdirTemp("", "inspector-examiner-output-")
	if err != nil {
		return Result{}, fmt.Errorf("creating examiner output directory: %w", err)
	}
	defer os.RemoveAll(outputDir)

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	image := opts.Image
	if image == "" {
		image = DefaultImage
	}
	network := opts.Network
	if network == "" {
		network = DefaultNetwork
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := NewContainerCommand(runCtx, ContainerOptions{
		InputDir:   prepared.Dir,
		OutputDir:  outputDir,
		Executable: executable,
		AppURL:     opts.AppURL,
		Model:      opts.Model,
		APIBaseURL: opts.APIBaseURL,
		Image:      image,
		Network:    network,
	})
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return Result{Kind: Refused, Message: fmt.Sprintf("examiner ran past its %s timeout; it could not be tested, not judged wrong", timeout)}, nil
		}
		return Result{Kind: Refused, Message: fmt.Sprintf("examiner agent did not complete: %v", err)}, nil
	}
	data, err := os.ReadFile(filepath.Join(outputDir, VerdictName))
	if err != nil {
		return Result{Kind: Refused, Message: fmt.Sprintf("examiner agent completed without a verdict: %v", err)}, nil
	}
	var result Result
	if err := jsonUnmarshalStrict(data, &result); err != nil {
		return Result{Kind: Refused, Message: fmt.Sprintf("examiner agent wrote an invalid verdict: %v", err)}, nil
	}
	checked, err := Classify(result.Verdict, prepared.TestChanges)
	if err != nil {
		return Result{Kind: Refused, Message: fmt.Sprintf("examiner agent wrote an invalid verdict: %v", err)}, nil
	}
	if checked.Kind != result.Kind {
		return Result{Kind: Refused, Message: "examiner agent verdict did not match its per-outcome results"}, nil
	}
	return checked, nil
}

// ContainerOptions names the only filesystem objects mounted into the examiner
// container. It intentionally has no application-source field.
type ContainerOptions struct {
	InputDir, OutputDir, Executable string
	AppURL, Model, APIBaseURL       string
	Image, Network                  string
}

func jsonUnmarshalStrict(data []byte, target any) error {
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

// NewContainerCommand builds the sealed Docker invocation. The input directory
// is read-only, the output directory is the sole writable host mount, and the
// static Inspector executable is the only program copied in.
func NewContainerCommand(ctx context.Context, opts ContainerOptions) *exec.Cmd {
	args := []string{
		"run", "--rm",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--mount", "type=bind,src=" + opts.InputDir + ",dst=/examiner/input,readonly",
		"--mount", "type=bind,src=" + opts.OutputDir + ",dst=/examiner/output",
		"--mount", "type=bind,src=" + opts.Executable + ",dst=/examiner/inspector,readonly",
		"--workdir", "/examiner",
		"--network", opts.Network,
		"--env", AnthropicAPIKeyEnvVar,
		opts.Image,
		"/examiner/inspector", "examiner-agent",
		"--input-dir", "/examiner/input",
		"--output-dir", "/examiner/output",
		"--app-url", opts.AppURL,
		"--model", opts.Model,
	}
	if opts.APIBaseURL != "" {
		args = append(args, "--api-base-url", opts.APIBaseURL)
	}
	return exec.CommandContext(ctx, "docker", args...)
}
