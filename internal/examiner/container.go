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
	// RuntimeImage is built locally from runtime/Dockerfile. The examiner never
	// accepts caller-selected executable content or a project image.
	RuntimeImage   = "inspector-examiner-agent:local"
	DefaultNetwork = "bridge"
	DefaultTimeout = 10 * time.Minute
	DefaultBudget  = 12
)

// RunOptions configures the container that holds the examiner agent. It has no
// repository path by design: app source cannot be mounted into this container.
type RunOptions struct {
	Inputs  Inputs
	AppURL  string
	Model   string
	Network string
	Timeout time.Duration
	Budget  int
	Stdout  io.Writer
	Stderr  io.Writer
}

// Run starts the sealed agent and returns its record-derived verdict. It writes
// only to temporary directories owned by Inspector, never to the project being
// judged.
func Run(ctx context.Context, opts RunOptions) (Result, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	prepared, err := PrepareInputsContext(runCtx, opts.Inputs)
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
	runtimeUser, err := localDockerRuntimeUser(runCtx)
	if err != nil {
		return Result{Kind: Refused, Message: fmt.Sprintf("no usable container runtime: %v", err)}, nil
	}
	if err := ensureRuntimeImage(runCtx); err != nil {
		return Result{Kind: Refused, Message: err.Error()}, nil
	}

	outputDir, verdictPath, cleanupOutput, err := prepareOutput()
	if err != nil {
		return Result{}, err
	}
	defer cleanupOutput()

	network := opts.Network
	if network == "" {
		network = DefaultNetwork
	}
	budget := opts.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}

	cmd := NewContainerCommand(runCtx, ContainerOptions{
		InputDir:  prepared.Dir,
		OutputDir: outputDir,
		AppURL:    opts.AppURL,
		Model:     opts.Model,
		Network:   network,
		Timeout:   timeout,
		Budget:    budget,
		User:      runtimeUser,
	})
	defer cmd.Cleanup()
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	runErr := cmd.Run()
	if killErr := cmd.KillError(); killErr != nil {
		return Result{Kind: Refused, Message: fmt.Sprintf("examiner container could not be stopped: %v; it may still be running", killErr)}, nil
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return Result{Kind: Refused, Message: fmt.Sprintf("examiner ran past its %s timeout; it could not be tested, not judged wrong", timeout)}, nil
	}
	if runErr != nil {
		return Result{Kind: Refused, Message: fmt.Sprintf("examiner agent did not complete: %v", runErr)}, nil
	}
	data, err := os.ReadFile(verdictPath)
	if err != nil {
		return Result{Kind: Refused, Message: fmt.Sprintf("examiner agent completed without a verdict; userns-remap may be unsupported on this host (see inspector#32): %v", err)}, nil
	}
	var result Result
	if err := jsonUnmarshalStrict(data, &result); err != nil {
		return Result{Kind: Refused, Message: fmt.Sprintf("examiner agent wrote an invalid verdict: %v", err)}, nil
	}
	return result, nil
}

func ensureRuntimeImage(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", RuntimeImage)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("checking examiner runtime image: %w", ctx.Err())
		}
		return fmt.Errorf("examiner runtime image %q is not available; build it locally with internal/examiner/runtime/build.sh before examining (the command never builds or pulls at examination time): %s", RuntimeImage, strings.TrimSpace(string(output)))
	}
	return nil
}

func prepareOutput() (string, string, func(), error) {
	dir, err := os.MkdirTemp("", "inspector-examiner-output-")
	if err != nil {
		return "", "", nil, fmt.Errorf("creating examiner output directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, VerdictName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("creating examiner verdict: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("creating examiner verdict: %w", err)
	}
	return dir, path, cleanup, nil
}

// ContainerOptions names the only filesystem objects mounted into the examiner
// container. It intentionally has no application-source or executable field.
type ContainerOptions struct {
	InputDir, OutputDir string
	AppURL, Model       string
	Network             string
	Timeout             time.Duration
	Budget              int
	User                string
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

// NewContainerCommand builds the sealed Docker invocation. The locally-built
// trusted image contains the agent. Inputs and output are the sole host mounts.
func NewContainerCommand(ctx context.Context, opts ContainerOptions) *container.Cmd {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	budget := opts.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	user := opts.User
	if user == "" {
		user = containerHostUser()
	}
	args := []string{
		"--pull", "never",
		"--read-only",
		"--userns", "host",
		"--user", user,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--mount", "type=bind,src=" + opts.InputDir + ",dst=/examiner/input,readonly",
		"--mount", "type=bind,src=" + opts.OutputDir + ",dst=/examiner/output",
		"--workdir", "/examiner",
		"--network", opts.Network,
		"--env", AnthropicAPIKeyEnvVar,
		"--entrypoint", "/usr/local/bin/examiner-agent",
		RuntimeImage,
		"--input-dir", "/examiner/input",
		"--output-dir", "/examiner/output",
		"--app-url", opts.AppURL,
		"--model", opts.Model,
		"--timeout", timeout.String(),
		"--budget", fmt.Sprintf("%d", budget),
	}
	return container.NewCommand(ctx, "inspector-examiner-", args...)
}

func localDockerRuntimeUser(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", container.ErrNotInstalled
	}
	endpoint, err := dockerOutput(ctx, "reading Docker endpoint", "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}")
	if err != nil {
		return "", err
	}
	var address string
	if err := json.Unmarshal(bytes.TrimSpace(endpoint), &address); err != nil {
		return "", fmt.Errorf("reading Docker endpoint: invalid response: %w", err)
	}
	if !strings.HasPrefix(address, "unix://") && !strings.HasPrefix(address, "npipe://") {
		return "", fmt.Errorf("remote Docker endpoint %q cannot safely use host bind mounts", address)
	}
	if err := container.EnsureAvailableContext(ctx); err != nil {
		return "", err
	}
	output, err := dockerOutput(ctx, "reading Docker security options", "info", "--format", "{{json .SecurityOptions}}")
	if err != nil {
		return "", err
	}
	var securityOptions []string
	if err := json.Unmarshal(bytes.TrimSpace(output), &securityOptions); err != nil {
		return "", fmt.Errorf("reading Docker security options: invalid response: %w", err)
	}
	for _, option := range securityOptions {
		for _, field := range strings.Split(option, ",") {
			if field == "rootless" || field == "name=rootless" {
				return "0:0", nil
			}
		}
	}
	return containerHostUser(), nil
}

func dockerOutput(ctx context.Context, operation string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err == nil {
		return output, nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: %w", operation, ctx.Err())
	}
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = err.Error()
	}
	return nil, fmt.Errorf("%s: %s", operation, detail)
}
