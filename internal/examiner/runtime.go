//go:build !examiner_agent

package examiner

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/inactdev/inspector/internal/container"
)

const (
	agentLinuxAMD64SHA256 = "91f3ac5f0c59bfc011691a8b41c9db69878e47f47e971939767f6dd9f72bb8dd"
	agentLinuxARM64SHA256 = "5ac8bef7a1df423c52c244f80a8416a6b72cb7bcd0730640a6b34f4f55d92be1"
)

//go:embed runtime/examiner-agent-linux-amd64.gz
var agentLinuxAMD64 []byte

//go:embed runtime/examiner-agent-linux-arm64.gz
var agentLinuxARM64 []byte

func prepareAgentExecutable(ctx context.Context) (string, func(), error) {
	target, err := dockerServerTarget(ctx)
	if err != nil {
		return "", func() {}, err
	}
	var compressed []byte
	var expectedHash string
	switch target {
	case "linux/amd64":
		compressed, expectedHash = agentLinuxAMD64, agentLinuxAMD64SHA256
	case "linux/arm64":
		compressed, expectedHash = agentLinuxARM64, agentLinuxARM64SHA256
	default:
		return "", func() {}, fmt.Errorf("Docker server platform %q has no bundled examiner agent", target)
	}
	actualHash := sha256.Sum256(compressed)
	if hex.EncodeToString(actualHash[:]) != expectedHash {
		return "", func() {}, fmt.Errorf("bundled examiner agent for %s failed its integrity check", target)
	}

	dir, err := os.MkdirTemp("", "inspector-examiner-agent-")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating examiner agent directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("opening bundled examiner agent for %s: %w", target, err)
	}
	path := filepath.Join(dir, "examiner-agent")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
	if err != nil {
		reader.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("creating examiner agent for %s: %w", target, err)
	}
	_, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	readerErr := reader.Close()
	if copyErr != nil || closeErr != nil || readerErr != nil {
		cleanup()
		for _, candidate := range []error{copyErr, closeErr, readerErr} {
			if candidate != nil {
				return "", func() {}, fmt.Errorf("extracting bundled examiner agent for %s: %w", target, candidate)
			}
		}
	}
	return path, cleanup, nil
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

func dockerServerTarget(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("reading Docker server platform: %w", ctx.Err())
		}
		return "", fmt.Errorf("reading Docker server platform: %w: %s", err, strings.TrimSpace(string(output)))
	}
	target := strings.TrimSpace(string(output))
	switch target {
	case "linux/x86_64":
		target = "linux/amd64"
	case "linux/aarch64":
		target = "linux/arm64"
	}
	return target, nil
}
