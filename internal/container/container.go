// Package container runs a project's check command inside Docker rather
// than on the host, so the file system and network it can reach are
// declared up front instead of inherited from whoever invoked inspector.
// See README.md for why, and issue #13 for the decisions recorded here.
package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WorkspaceDir is where the repo is bind-mounted inside the container,
// and the check command's working directory. Fixed rather than
// configurable: the container has nothing else to run from.
const WorkspaceDir = "/workspace"

// ErrNotInstalled means the docker CLI itself is not on PATH.
var ErrNotInstalled = errors.New("docker not found on PATH - install Docker (https://docs.docker.com/get-docker/) so check commands can run in a container instead of on this machine")

// ErrDaemonUnreachable means the docker CLI is present but can't reach a
// daemon - installed without being started, a remote context that's
// down, or a permissions problem.
var ErrDaemonUnreachable = errors.New("docker is installed but its daemon is not reachable - start Docker (e.g. Docker Desktop, or `sudo systemctl start docker`) and try again")

// EnsureAvailable confirms a usable container runtime is present. Call
// it before ever attempting to run a check, so a missing or unusable
// runtime is refused with a specific, actionable reason instead of
// inspector quietly running the check command on the host - the one
// thing this package exists to prevent.
func EnsureAvailable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return ErrNotInstalled
	}
	// `docker info` talks to the daemon and fails clearly (nonzero exit)
	// when there isn't one to talk to, without side effects.
	if err := exec.Command("docker", "info").Run(); err != nil {
		return ErrDaemonUnreachable
	}
	return nil
}

// Run describes one containerized check command.
type Run struct {
	// RepoRoot is bind-mounted read-write at WorkspaceDir. Nothing else
	// on the host is reachable from inside the container.
	RepoRoot string
	// Command is run with `sh -c` inside the container, from WorkspaceDir.
	Command string
	// Image is the project's own container image, declared in
	// .inspector.json - inspector never guesses one, the same way it
	// never guesses a check command: the project's toolchain has to be
	// in it or nothing runs.
	Image string
	// Network, when false (the default), runs the container with no
	// network access at all. A check command that needs the network -
	// installing dependencies is the common case - must opt in via
	// .inspector.json; see README.md for the tradeoff.
	Network bool
}

// Cmd is one containerized run in progress: the *exec.Cmd for the
// attached `docker run` client process, plus what Started needs to tell
// a run that never got off the ground apart from one that did, plus
// whatever KillError has to report about stopping it.
type Cmd struct {
	*exec.Cmd
	cidFile string

	mu      sync.Mutex
	killErr error
}

// New builds the *exec.Cmd for running r inside a fresh, single-use,
// named container. The caller sets Stdout/Stderr and calls Run/Wait
// exactly as with any other exec.Cmd; ctx's deadline governs the run the
// same way it would a host process.
//
// Cancel stops the container itself via `docker kill`, not the local
// `docker run` process - `docker run`'s signal proxy only forwards
// SIGINT/SIGTERM into the container, so a SIGKILL aimed at the client
// process would only detach from a container that keeps running.
// Once the container's own init process is killed, the kernel tears
// down its whole PID namespace - every process it forked dies with it in
// one step, which is what the host-side process-group kill (killed along
// with check.go's old direct `sh` execution) was already reaching for.
// The attached `docker run` client then observes the container stop and
// exits on its own, carrying the container's exit status using the same
// 128+N convention a directly-killed shell command already used, so
// classifyResult needed no container-specific case.
func New(ctx context.Context, r Run) *Cmd {
	args := []string{
		"-v", r.RepoRoot + ":" + WorkspaceDir,
		"-w", WorkspaceDir,
	}
	if !r.Network {
		args = append(args, "--network", "none")
	}
	args = append(args, r.Image, "sh", "-c", r.Command)
	return NewCommand(ctx, "inspector-check-", args...)
}

// NewCommand builds a managed Docker run from the supplied arguments. Context
// cancellation kills the container and every process inside it, not only the
// attached Docker client.
func NewCommand(ctx context.Context, namePrefix string, args ...string) *Cmd {
	name := namePrefix + randomHex(8)
	cidFile := filepath.Join(os.TempDir(), name+".cid")
	dockerArgs := []string{"run", "--rm", "--name", name, "--cidfile", cidFile}
	dockerArgs = append(dockerArgs, args...)

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	cmd.WaitDelay = 5 * time.Second

	c := &Cmd{Cmd: cmd, cidFile: cidFile}
	cmd.Cancel = func() error { return c.kill(name) }
	return c
}

func (c *Cmd) kill(name string) error {
	err := killContainer(name)
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		c.mu.Lock()
		c.killErr = err
		c.mu.Unlock()
	}
	return err
}

// KillError reports docker's own refusal to stop the container after
// ctx's deadline fired, carrying docker's message, and nil when no kill
// was needed or the kill worked. It is the authoritative answer to "did
// this container actually get stopped": what Run returns is not, because
// exec.Cmd.Wait prefers the `docker run` client's own exit status and
// only falls back to Cancel's error when that status is clean - so
// precisely when the container is left running and the client had to be
// SIGKILLed, Wait discards the reason. Read it after Run/Wait returns.
func (c *Cmd) KillError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.killErr
}

// Started reports whether the container was ever created, which is only
// meaningful after Run/Wait has returned. False means `docker run` never
// got as far as running spec.Command at all - a bad image name, a
// registry it couldn't reach, a daemon that vanished mid-call - so
// whatever exit code the docker CLI itself produced (Docker's own
// convention reserves 125 for exactly this, "the error is with docker
// itself") is not a verdict on the check command and must not be read as
// one. The cidfile is Docker's own signal for this: it's written at
// container creation, before the command inside it ever runs, so its
// absence means creation itself never happened.
func (c *Cmd) Started() bool {
	data, err := os.ReadFile(c.cidFile)
	return err == nil && strings.TrimSpace(string(data)) != ""
}

// Cleanup removes the cidfile New asked Docker to write. The docker CLI
// deletes it only when container creation failed, so every run that got
// as far as creating one leaves the file behind otherwise. Callers
// should defer this right after New - it is idempotent, but it removes
// what Started reads, so it must not run before Started has been
// consulted.
func (c *Cmd) Cleanup() {
	os.Remove(c.cidFile)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read on any of Go's supported platforms only
		// fails if the OS entropy source itself is broken, which is a
		// machine-level problem no caller here could act on - the
		// container name just needs to be unlikely to collide, not
		// secret, so falling back to a fixed, still-probably-unique
		// suffix keeps this path from ever panicking.
		return "0000000000000000"
	}
	return hex.EncodeToString(buf)
}

// killContainer force-stops the named container. Mirrors the contract
// exec.Cmd.Cancel documents: nil means the container is being stopped,
// os.ErrProcessDone means there was nothing to do.
//
// Only one docker failure means that: "No such container", which with
// --rm is what a container that already exited on its own right as the
// deadline fired looks like - the same race check.go's old
// killProcessGroup had to account for, and narrowed just as carefully
// there (ESRCH and a verified EPERM, not any errno). Every other
// failure - "is not running", a daemon that stopped answering, a
// permissions problem - is a real error carrying docker's own message,
// since os.ErrProcessDone would claim a container that may still be
// running against the bind-mounted repo had been dealt with.
//
// The returned error deliberately does not wrap the *exec.ExitError
// from `docker kill` itself: that is a different process from the one
// being waited on, and classifyResult reads any *exec.ExitError it can
// find as the check container's own verdict.
func killContainer(name string) error {
	out, err := exec.Command("docker", "kill", name).CombinedOutput()
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(string(out))
	if message == "" {
		message = err.Error()
	}
	if strings.Contains(strings.ToLower(message), "no such container") {
		return os.ErrProcessDone
	}
	return fmt.Errorf("docker kill %s: %s", name, message)
}
