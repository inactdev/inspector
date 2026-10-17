package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrRemoteDocker means the resolved Docker endpoint is not a local
// socket or pipe. ResolveUser refuses rather than guessing: a remote
// daemon's bind mounts and uid/gid numbers belong to a different
// machine's filesystem and user namespace than the one HostUser reads,
// so matching them to "the inviting host user" would be meaningless at
// best and could ask for a privileged uid on the remote host at worst.
var ErrRemoteDocker = errors.New("remote Docker endpoint cannot safely use host bind mounts")

// ResolveUser decides the docker run `--user` value New should use so
// the check container runs as the inviting host user rather than the
// image's own default (commonly root): a runaway or hostile check
// command then owns nothing on the host it didn't already own.
//
// A rootless Docker daemon already maps a container's own root to the
// host's unprivileged user through its own user namespace, so asking
// it for the host uid/gid again would name a uid that is root *inside*
// that namespace - rootless Docker refuses to run as that, same as
// rootful Docker refuses an arbitrary uid with no meaning. Detected via
// `docker info`'s SecurityOptions and left as "0:0" (root), which from
// outside the rootless namespace already is the unprivileged host user.
//
// Ported from internal/examiner's identical, tested need for the same
// decision (its sealed agent container); keep the two in step rather
// than letting them drift the way a hand-duplicated list already did
// once in this project (see CLAUDE.md on required-tools.sh).
func ResolveUser(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", ErrNotInstalled
	}
	address, err := dockerEndpoint(ctx)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(address, "unix://") && !strings.HasPrefix(address, "npipe://") {
		return "", fmt.Errorf("%w: %q", ErrRemoteDocker, address)
	}
	if err := EnsureAvailableContext(ctx); err != nil {
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
	return HostUser(), nil
}

func dockerEndpoint(ctx context.Context) (string, error) {
	contextName := strings.TrimSpace(os.Getenv("DOCKER_CONTEXT"))
	if contextName == "" {
		if address := strings.TrimSpace(os.Getenv("DOCKER_HOST")); address != "" {
			return address, nil
		}
	}
	args := []string{"context", "inspect", "--format", "{{json .Endpoints.docker.Host}}"}
	if contextName != "" {
		args = []string{"context", "inspect", contextName, "--format", "{{json .Endpoints.docker.Host}}"}
	}
	output, err := dockerOutput(ctx, "reading Docker endpoint", args...)
	if err != nil {
		return "", err
	}
	var address string
	if err := json.Unmarshal(bytes.TrimSpace(output), &address); err != nil {
		return "", fmt.Errorf("reading Docker endpoint: invalid response: %w", err)
	}
	return address, nil
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
