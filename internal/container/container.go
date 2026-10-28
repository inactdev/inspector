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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// WorkspaceDir is where the repo is bind-mounted inside the container,
// and the check command's working directory. Fixed rather than
// configurable: the container has nothing else to run from.
const WorkspaceDir = "/workspace"

// DefaultMemory, DefaultCPUs, and DefaultPIDsLimit match Fabrica's own
// worker box (`--memory 2g --cpus 2 --pids-limit 512`), so a runaway
// test or fork bomb in a project's check command cannot exhaust the
// host the same way issue #43 found Fabrica's worker already guards
// against. New applies these whenever a Run leaves the matching field
// at its Go zero value; a project raises or lowers any of them via
// .inspector.json.
const (
	DefaultMemory    = "2g"
	DefaultCPUs      = "2"
	DefaultPIDsLimit = 512
)

// Limits bounds the cgroup resources a container Run may use. A zero
// field (the Go zero value, meaning "not set") falls back to the
// matching Default* constant above rather than running unbounded.
type Limits struct {
	// Memory is docker run's own --memory value, e.g. "2g".
	Memory string
	// CPUs is docker run's own --cpus value, e.g. "2".
	CPUs string
	// PIDsLimit is docker run's own --pids-limit value.
	PIDsLimit int
}

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
	return EnsureAvailableContext(context.Background())
}

// EnsureAvailableContext confirms a usable container runtime while respecting
// the caller's cancellation or deadline.
func EnsureAvailableContext(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return ErrNotInstalled
	}
	// `docker info` talks to the daemon and fails clearly (nonzero exit)
	// when there isn't one to talk to, without side effects.
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("checking Docker availability: %w", ctx.Err())
		}
		return ErrDaemonUnreachable
	}
	return nil
}

// Run describes one containerized check command.
type Run struct {
	// WorkspaceRoot is bind-mounted read-write at WorkspaceDir, and is
	// the only writable thing the container can reach apart from
	// WritableMounts and its own throwaway file system. For a project
	// check it is ResolveWorkspace's workspace rather than the repo
	// itself: the commit under check arrives through SourceMounts,
	// read-only (issue #27). Nothing on the host is reachable from
	// inside the container except this and the three mount lists below.
	WorkspaceRoot string
	// SourceMounts carry the commit under check into the workspace,
	// read-only - one mount per tracked top-level entry of the repo.
	// See Workspace and ResolveWorkspace.
	SourceMounts []Mount
	// WritableMounts are the host paths the check command may write:
	// the repo root's own untracked, gitignored leftovers (build
	// output, installed dependencies), which are not what the commit
	// under check is made of. See Workspace.
	WritableMounts []Mount
	// ReadOnlyMounts are the few host paths that have to be readable
	// alongside the workspace for the check command to work at all,
	// each at a fixed absolute path inside the container and none of
	// them writable. In practice this is what ResolveGitMounts returns:
	// a linked worktree's shared git directory, which lives outside the
	// repo root, plus a sanitized stand-in for its config. Unlike
	// SourceMounts these are declared to git as safe.directory below,
	// because they are git directories rather than paths inside one.
	ReadOnlyMounts []Mount
	// Command is run with `sh -c` inside the container, from WorkspaceDir.
	Command string
	// Image is the project's own container image, built from the
	// Dockerfile the project commits for it - inspector never guesses
	// one, the same way it never guesses a check command: the project's
	// toolchain has to be in it or nothing runs. See BuildProjectImage.
	Image string
	// Limits bounds the container's memory, CPU, and process count.
	// See Limits and the Default* constants above.
	Limits Limits
	// User is the docker run `--user` value ("uid:gid") the container
	// runs as. Empty leaves the image's own default user (commonly
	// root). See ResolveUser - New never guesses this on its own.
	User string
}

// containerCreateGrace bounds how long a cancelled run waits for Docker
// to finish creating the container it was still working on when the
// deadline fired, on top of whatever is left of the run's own deadline
// (issue #66). Never indefinite: a creation that failed outright writes
// no container id and never will. A var rather than a const only so this
// package's own tests can shorten it; nothing changes it at runtime.
var containerCreateGrace = 30 * time.Second

// containerCreatePoll is how often the cidfile is looked at while waiting
// out containerCreateGrace.
const containerCreatePoll = 50 * time.Millisecond

// dockerCleanupTimeout bounds each `docker kill`/`rm`/`inspect` run the
// cleanup path makes, so a daemon that stopped answering costs a bounded
// wait rather than hanging the run that is already being torn down.
const dockerCleanupTimeout = 5 * time.Second

// Cmd is one containerized run in progress: the *exec.Cmd for the
// attached `docker run` client process, plus what Started needs to tell
// a run that never got off the ground apart from one that did, plus
// whatever KillError and StopNote have to report about stopping it.
type Cmd struct {
	*exec.Cmd
	cidFile string
	name    string
	// deadline is the run's own deadline, when it has one, so the kill
	// path can bound its wait for a container Docker is still creating
	// by what is left of it plus containerCreateGrace.
	deadline time.Time

	mu          sync.Mutex
	cancelled   bool
	cleanupDone bool
	killErr     error
	stopNote    string
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
//
// A deadline that fires before there is any container to kill - Docker
// can take seconds to create one on a cold machine - waits for it rather
// than letting the check command start behind the kill and run to
// completion (issue #66). See kill, awaitContainerID and StopNote.
func New(ctx context.Context, r Run) *Cmd {
	args := []string{
		"-v", r.WorkspaceRoot + ":" + WorkspaceDir,
		"-w", WorkspaceDir,
	}
	// Docker applies a container's mounts in lexicographic order of
	// their destination rather than flag order, so a nested destination
	// always lands on top of its already-mounted parent - the behavior
	// ResolveGitMounts already relies on, verified live here and in
	// Fabrica for the file it lays over a mounted git directory. These
	// are emitted parent-first regardless, so the result does not
	// depend on that sort. Every source here exists: ResolveWorkspace
	// read it off disk, which matters because -v would otherwise create
	// a missing source as an empty directory instead of failing.
	for _, mount := range r.SourceMounts {
		args = append(args, "-v", mount.Source+":"+mount.Target+":ro")
	}
	for _, mount := range r.WritableMounts {
		args = append(args, "-v", mount.Source+":"+mount.Target)
	}
	for _, mount := range r.ReadOnlyMounts {
		args = append(args, "-v", mount.Source+":"+mount.Target+":ro")
	}
	// Everything mounted here is owned by whoever ran inspector, while
	// the image picks its own user - so on Linux, where bind-mounted
	// files keep their host ownership, git would refuse to look at the
	// repository at all ("detected dubious ownership", verified) unless
	// User (ResolveUser) already matches that ownership. Declared
	// unconditionally anyway, scoped to the workspace and whatever git
	// directory was mounted beside it - the only paths git has to trust
	// as a repository - rather than widened to `*` or to every mount
	// above: git's own documented escape hatch for
	// this, kept as defense in depth for whatever ownership mismatch a
	// given host/Docker combination still produces (rootless remapping,
	// a macOS filesystem-sharing backend, ResolveUser falling back to
	// "0:0"), without trusting a directory inspector never mounted. This
	// has to be environment, not a config file - git honors
	// safe.directory only from protected configuration, never from a
	// repository's own config file.
	safeDirs := []string{WorkspaceDir}
	for _, mount := range r.ReadOnlyMounts {
		safeDirs = append(safeDirs, mount.Target)
	}
	// The workspace is its own file system and every source mount above
	// is another, so WorkspaceDir is a mount point - and git stops its
	// upward search for a repository at a file system boundary unless
	// told not to ("Stopping at filesystem boundary
	// (GIT_DISCOVERY_ACROSS_FILESYSTEM not set)"). Without this, a check
	// command that runs git from any subdirectory of the repo would be
	// told it is not in a repository at all, which is the failure issue
	// #31 already fixed once by a different route. Nothing but the
	// workspace and its mounts is in this container to be discovered, so
	// crossing that boundary can only find the repository that is
	// supposed to be found. git(1)'s own "Environment Variables"
	// section documents this as affecting that upward search only -
	// never a repository named explicitly - so it cannot reach how the
	// git directory mounted by ResolveGitMounts is read.
	args = append(args, "-e", "GIT_DISCOVERY_ACROSS_FILESYSTEM=1")
	args = append(args, "-e", fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(safeDirs)))
	for i, dir := range safeDirs {
		args = append(args,
			"-e", fmt.Sprintf("GIT_CONFIG_KEY_%d=safe.directory", i),
			"-e", fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, dir),
		)
	}

	memory := r.Limits.Memory
	if memory == "" {
		memory = DefaultMemory
	}
	cpus := r.Limits.CPUs
	if cpus == "" {
		cpus = DefaultCPUs
	}
	pidsLimit := r.Limits.PIDsLimit
	if pidsLimit == 0 {
		pidsLimit = DefaultPIDsLimit
	}
	args = append(args,
		"--memory", memory,
		"--cpus", cpus,
		"--pids-limit", strconv.Itoa(pidsLimit),
	)

	if r.User != "" {
		// Many images default HOME to a directory only their own
		// default user (commonly root) can write - /root is the usual
		// case - which would otherwise break any check command whose
		// toolchain writes a cache there (go, npm, pip) the moment it
		// runs as a different, unprivileged uid. /tmp is writable by
		// everyone in every image this has been tried against and is
		// thrown away with the container, so it is the one path safe
		// to assume regardless of which uid ends up running as.
		args = append(args, "--user", r.User, "-e", "HOME=/tmp")
	}

	// Denied, with no opt-in to grant it (issue #47): the same
	// containment this package exists for would otherwise stop at the
	// repo's own files and let the network stand between a hostile check
	// and exfiltrating whatever it found. A check command needs nothing
	// from the network now that the project's dependencies are baked
	// into the image BuildProjectImage builds from the project's own
	// committed Dockerfile - installing them at check time is what the
	// retired "network": true opt-in existed for.
	args = append(args, "--network", "none")

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
	cmd.WaitDelay = dockerCleanupTimeout

	c := &Cmd{Cmd: cmd, cidFile: cidFile, name: name}
	if deadline, ok := ctx.Deadline(); ok {
		c.deadline = deadline
	}
	cmd.Cancel = c.kill
	return c
}

// kill stops the run ctx just cancelled. It blocks for as long as it
// takes to get hold of the container, which is safe by construction:
// os/exec starts its WaitDelay timer only once Cancel returns, so the
// delay window cannot open while this is still looking for something to
// kill.
func (c *Cmd) kill() error {
	c.mu.Lock()
	c.cancelled = true
	c.mu.Unlock()

	id, waited, created := c.awaitContainerID()
	if !created {
		return c.stopWithoutContainer(waited)
	}
	if waited > 0 {
		// Worth saying out loud: the container that was stopped only
		// existed because the kill path waited for it, and without that
		// wait its check command would have run on past the deadline.
		c.note(fmt.Sprintf("the deadline fired before Docker had created the check container; inspector waited %s for it to exist and then stopped it", waited.Round(time.Millisecond)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()
	err := c.stopContainer(ctx, id)
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		c.mu.Lock()
		c.killErr = err
		c.mu.Unlock()
	}
	return err
}

// stopContainer stops the container id names, in whichever state Docker
// has it in.
func (c *Cmd) stopContainer(ctx context.Context, id string) error {
	err := killContainer(ctx, id)
	if !errors.Is(err, errContainerNotRunning) {
		return err
	}
	// Created, but not started yet - `docker kill` has no process to
	// signal, and the attached `docker run` client is about to start it
	// (issue #66). Removing it by force is the one stop that works in
	// either state, so even a container that starts between these two
	// calls is taken down rather than left to run past the deadline.
	rmErr := removeContainer(ctx, id)
	if rmErr == nil || errors.Is(rmErr, os.ErrProcessDone) {
		c.note("the deadline fired while Docker had the check container created but not yet started; inspector removed it before its check command could run")
		return rmErr
	}
	return fmt.Errorf("%w (docker kill had already reported it as not running)", rmErr)
}

// stopWithoutContainer is the last resort for a deadline that fired
// before Docker ever reported a container and where none appeared within
// awaitContainerID's whole budget: a `docker create` that is hung, or one
// that failed. The `docker run` client is then the only handle left, and
// killing it also keeps it from starting a container the daemon may still
// be finishing - but the create request is already with the daemon, so
// the kill is followed by a removal of whatever did appear and then by
// asking docker outright whether anything of this run is still running.
// Per the ruling on issue #66 that last answer, and only it, decides
// whether this counts as a kill inspector could not carry out.
func (c *Cmd) stopWithoutContainer(waited time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()

	// A client that has already exited is not a failure here: there is
	// simply nothing left to detach from.
	client := "Docker's own client had already exited"
	if c.Process != nil && c.Process.Kill() == nil {
		client = "inspector killed Docker's own client"
	}
	what := fmt.Sprintf("Docker never reported a check container within %s of the deadline, so %s", waited.Round(time.Millisecond), client)

	// Docker may have finished creating the container in the window
	// between the last look and that kill, so aim at whatever is there.
	target := c.stopTarget()
	rmErr := removeContainer(ctx, target)
	switch {
	case rmErr == nil:
		c.note(what + ", and force-removed the container it turned out to have created after all")
		return nil
	case errors.Is(rmErr, os.ErrProcessDone):
		c.note(what + ", and there was no container to remove - the check command never ran")
		return nil
	}

	running, inspectErr := containerRunning(ctx, target)
	if running {
		err := fmt.Errorf("%s, and a container of this run is still running after an attempt to force-remove it: %w", what, rmErr)
		c.mu.Lock()
		c.killErr = err
		c.mu.Unlock()
		return err
	}
	detail := rmErr.Error()
	if inspectErr != nil {
		detail = fmt.Sprintf("%v, and docker could not say whether one is running either: %v", rmErr, inspectErr)
	}
	c.note(fmt.Sprintf("%s, and removing any container of this run failed: %s - no container of this run is confirmed running", what, detail))
	return nil
}

// awaitContainerID resolves the container id Docker writes to the
// cidfile at creation, waiting for it when the deadline fired while
// `docker run` was still creating the container - the race issue #66
// found on a cold CI runner, where `docker kill` had nothing to kill and
// the check command then ran to completion anyway. Reports how long it
// waited, and whether a container id ever appeared.
//
// Bounded by whatever is left of this run's own deadline plus
// containerCreateGrace, never indefinite, and cut short the moment the
// `docker run` client itself is gone: a creation that failed outright (a
// bad image, a daemon that went away) leaves no cidfile and never will.
func (c *Cmd) awaitContainerID() (id string, waited time.Duration, created bool) {
	if id := c.containerID(); id != "" {
		return id, 0, true
	}
	budget := containerCreateGrace
	if !c.deadline.IsZero() {
		if remaining := time.Until(c.deadline); remaining > 0 {
			budget += remaining
		}
	}

	start := time.Now()
	for {
		if c.clientExited() {
			// One last look: the client may have written the cidfile on
			// its way out.
			if id := c.containerID(); id != "" {
				return id, time.Since(start), true
			}
			return "", time.Since(start), false
		}
		if elapsed := time.Since(start); elapsed >= budget {
			return "", elapsed, false
		}
		time.Sleep(containerCreatePoll)
		if id := c.containerID(); id != "" {
			return id, time.Since(start), true
		}
	}
}

// stopTarget is what a cleanup command is aimed at: the container id when
// Docker has written one, since a command aimed at the exact container
// cannot be confused by anything else answering to the name, and this
// run's own container name otherwise - New gives every run a unique one.
func (c *Cmd) stopTarget() string {
	if id := c.containerID(); id != "" {
		return id
	}
	return c.name
}

// containerID reads the container id Docker wrote at creation, or "" when
// there is none to read yet. See Started for why the cidfile is the
// signal for that.
func (c *Cmd) containerID() string {
	data, err := os.ReadFile(c.cidFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// clientExited reports whether the `docker run` client process is gone.
// os.Process.Signal is the way to ask: it reports ErrProcessDone once
// Wait has reaped the process, under the Process's own lock, whereas
// reading Cmd.ProcessState from here would race with the Wait that
// writes it on another goroutine. Signal 0 is not delivered to anything.
func (c *Cmd) clientExited() bool {
	if c.Process == nil {
		return true
	}
	return errors.Is(c.Process.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

// note records, for the person reading this run's report, what inspector
// had to do to stop a container that was not simply there and running
// when the deadline fired.
func (c *Cmd) note(note string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopNote != "" {
		c.stopNote += "; "
	}
	c.stopNote += note
}

// StopNote describes in words what stopping this run actually took, and
// is empty for the ordinary case - no cancellation, or a container that
// was there and running when the deadline fired. Unlike KillError it is
// not a failure: it is the difference between "the check timed out and
// was stopped" and "the check timed out and inspector had to kill
// Docker's own client to stop it", which a report must not blur. Read it
// after Run/Wait returns.
func (c *Cmd) StopNote() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopNote
}

func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

func (c *Cmd) Wait() error {
	err := c.Cmd.Wait()
	c.finalizeCleanup()
	return err
}

func (c *Cmd) finalizeCleanup() {
	c.mu.Lock()
	if !c.cancelled || c.cleanupDone {
		c.mu.Unlock()
		return
	}
	c.cleanupDone = true
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()
	// The last catch for a container the daemon created after the kill
	// path had already given up looking for one, which is the only way
	// one can still exist by now.
	err := removeContainer(ctx, c.stopTarget())

	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		c.killErr = nil
	} else {
		c.killErr = err
	}
}

// KillError reports docker's own refusal to stop the container after
// ctx's deadline fired, carrying docker's message, and nil when no kill
// was needed or cleanup was confirmed after the Docker client exited. A
// container that was created late and then stopped, or one Docker never
// reported at all and that is not running when asked, is not a refusal -
// those are StopNote's to report (issue #66). Read it after Run/Wait
// returns.
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
	return c.containerID() != ""
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

// errContainerNotRunning is docker's refusal to kill a container that
// exists but has no process to signal - which, for a container inspector
// created this instant, means Docker has created it and not started it
// yet. stopContainer removes such a container rather than reporting a
// kill it could not carry out (issue #66). Wrapped into the returned
// error rather than replacing it, so a caller that only reports the
// failure still carries docker's own message.
var errContainerNotRunning = errors.New("docker reports the container as not running")

// killContainer force-stops the container target names, by id or by name.
// Mirrors the contract exec.Cmd.Cancel documents: nil means the container
// is being stopped, os.ErrProcessDone means there was nothing to do.
//
// Only one docker failure means that: "No such container", which with
// --rm is what a container that already exited on its own right as the
// deadline fired looks like - the same race check.go's old
// killProcessGroup had to account for, and narrowed just as carefully
// there (ESRCH and a verified EPERM, not any errno). "Is not running" is
// the one other failure read for what it is, as errContainerNotRunning,
// and only so that the caller can remove a created-but-unstarted
// container instead of leaving it to start. Every other failure - a
// daemon that stopped answering, a permissions problem - is a real error
// carrying docker's own message, since os.ErrProcessDone would claim a
// container that may still be running against the bind-mounted repo had
// been dealt with.
//
// The returned error deliberately does not wrap the *exec.ExitError
// from `docker kill` itself: that is a different process from the one
// being waited on, and classifyResult reads any *exec.ExitError it can
// find as the check container's own verdict.
func killContainer(ctx context.Context, target string) error {
	return runDockerCleanup(ctx, "kill", target)
}

func removeContainer(ctx context.Context, target string) error {
	return runDockerCleanup(ctx, "rm", "--force", target)
}

func runDockerCleanup(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err == nil {
		return nil
	}
	message := dockerMessage(ctx, out, err)
	if isMissingContainer(message) {
		return os.ErrProcessDone
	}
	wrapped := fmt.Errorf("docker %s %s: %s", args[0], args[len(args)-1], message)
	if strings.Contains(strings.ToLower(message), "is not running") {
		return fmt.Errorf("%w: %w", errContainerNotRunning, wrapped)
	}
	return wrapped
}

// containerRunning asks docker outright whether the container target
// names is running. Reports false with a nil error for a container docker
// says is not there at all - that is an answer, not a failure to get one -
// and false with an error only when docker could not be asked, which is
// the one case a caller must not read as "nothing is running".
func containerRunning(ctx context.Context, target string) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", target).CombinedOutput()
	if err != nil {
		message := dockerMessage(ctx, out, err)
		if isMissingContainer(message) {
			return false, nil
		}
		return false, fmt.Errorf("docker inspect %s: %s", target, message)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// dockerMessage prefers docker's own words, falls back to the Go error
// when it said nothing, and names a cancelled or timed-out call for what
// it was rather than reporting whatever partial output it had produced.
func dockerMessage(ctx context.Context, out []byte, err error) string {
	if ctx.Err() != nil {
		return ctx.Err().Error()
	}
	if message := strings.TrimSpace(string(out)); message != "" {
		return message
	}
	return err.Error()
}

// isMissingContainer recognizes docker's two spellings for a container
// that is not there: `kill` and `rm` say "No such container", `inspect`
// says "No such object".
func isMissingContainer(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "no such container") || strings.Contains(lower, "no such object")
}
