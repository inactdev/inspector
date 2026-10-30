package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inactdev/inspector/internal/dockertest"
)

// requireDocker skips a test that genuinely needs a live container
// runtime, rather than failing on a machine without one - the same
// tolerance LoadConfig's own permission-bit test already extends to a
// root-run suite (internal/inspector/config_test.go).
//
// `alpine` is what every live test in this package runs except the git ones,
// which ask for gitImage themselves, and the one that deliberately names an
// image nobody has. A daemon that cannot obtain the image is not a usable
// runtime for them, so it is part of the same gate rather than an error
// arriving mid-test.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := EnsureAvailable(); err != nil {
		t.Skipf("no usable container runtime, skipping: %v", err)
	}
	dockertest.RequireImage(t, "alpine")
}

func TestEnsureAvailable_NotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	err := EnsureAvailable()
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("EnsureAvailable() = %v, want ErrNotInstalled", err)
	}
}

func TestEnsureAvailable_Live(t *testing.T) {
	requireDocker(t)

	if err := EnsureAvailable(); err != nil {
		t.Fatalf("EnsureAvailable() = %v, want nil - requireDocker just confirmed one is usable", err)
	}
}

func TestEnsureAvailableContext_DeadlineStopsDockerProbe(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexec /bin/sleep 10\n"), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := EnsureAvailableContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EnsureAvailableContext() = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("EnsureAvailableContext() returned after %s, want it bounded by the context", elapsed)
	}
}

func TestNew_BuildsExpectedArgs(t *testing.T) {
	// Pure argument construction - doesn't touch docker, so it runs
	// without a runtime present.
	cmd := New(context.Background(), Run{
		WorkspaceRoot: "/host/repo",
		Command:       "go test ./...",
		Image:         "golang:1.22",
	})

	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"docker run --rm",
		"-v /host/repo:" + WorkspaceDir,
		"-w " + WorkspaceDir,
		"--memory " + DefaultMemory,
		"--cpus " + DefaultCPUs,
		"--pids-limit 512",
		"--network none",
		"golang:1.22 sh -c go test ./...",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker args = %q, want it to contain %q", args, want)
		}
	}
}

func TestNew_SourceMountsAreReadOnlyAndWritableMountsAreNot(t *testing.T) {
	// Issue #27: the workspace is writable so an ordinary check can
	// build into its own tree, and the commit under check is laid over
	// it read-only so the same check cannot modify what it is judged
	// on. The `:ro` suffix on one list and its absence on the other is
	// the whole enforcement, so it is asserted literally.
	cmd := New(context.Background(), Run{
		WorkspaceRoot:  "/tmp/inspector-workspace-1",
		SourceMounts:   []Mount{{Source: "/host/repo/cmd", Target: WorkspaceDir + "/cmd"}},
		WritableMounts: []Mount{{Source: "/host/repo/node_modules", Target: WorkspaceDir + "/node_modules"}},
		Command:        "true",
		Image:          "alpine",
		User:           "1000:1000",
	})

	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"-v /tmp/inspector-workspace-1:" + WorkspaceDir,
		"-v /host/repo/cmd:" + WorkspaceDir + "/cmd:ro",
		"-v /host/repo/node_modules:" + WorkspaceDir + "/node_modules ",
	} {
		if !strings.Contains(args+" ", want) {
			t.Fatalf("docker args = %q, want it to contain %q", args, want)
		}
	}
	if strings.Contains(args, "/host/repo/node_modules:"+WorkspaceDir+"/node_modules:ro") {
		t.Fatalf("docker args = %q, want an untracked path mounted writable, not read-only", args)
	}
	// Per-entry mounts make WorkspaceDir a file system boundary, which
	// git refuses to search past when looking for a repository from a
	// subdirectory unless this says otherwise.
	if !strings.Contains(args, "GIT_DISCOVERY_ACROSS_FILESYSTEM=1") {
		t.Fatalf("docker args = %q, want git allowed to find the repository from a subdirectory of the workspace", args)
	}
	if !strings.Contains(args, "GIT_CONFIG_COUNT=1") {
		t.Fatalf("docker args = %q, want safe.directory declared for the workspace alone - source and writable mounts are paths inside a repository, not git directories", args)
	}
}

// TestNew_TermsMountIsReadOnlyAndNotTrustedAsAGitDirectory covers issue
// #69's mount: the terms folder arrives at the fixed TermsDir, read-only,
// and - unlike ReadOnlyMounts - is not declared to git as a safe
// directory, because it is configuration rather than a repository the
// check command has to read.
func TestNew_TermsMountIsReadOnlyAndNotTrustedAsAGitDirectory(t *testing.T) {
	cmd := New(context.Background(), Run{
		WorkspaceRoot: "/tmp/inspector-workspace-1",
		TermsMount:    Mount{Source: "/host/terms", Target: TermsDir},
		Command:       "sh " + TermsDir + "/check.sh",
		Image:         "alpine",
		User:          "1000:1000",
	})

	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "-v /host/terms:"+TermsDir+":ro") {
		t.Fatalf("docker args = %q, want the terms folder mounted read-only at %s", args, TermsDir)
	}
	// The working directory is still the repository's workspace: -terms
	// moves where the terms live, not where the check command runs.
	if !strings.Contains(args, "-w "+WorkspaceDir) {
		t.Fatalf("docker args = %q, want the check command still run from %s", args, WorkspaceDir)
	}
	if !strings.Contains(args, "GIT_CONFIG_COUNT=1") || strings.Contains(args, "GIT_CONFIG_VALUE_1="+TermsDir) {
		t.Fatalf("docker args = %q, want safe.directory left to the workspace and real git mounts", args)
	}
}

// A run with no terms folder mounts nothing extra - the ordinary case,
// where the terms are the repository's own and already in the workspace.
func TestNew_NoTermsMountWithoutATermsFolder(t *testing.T) {
	cmd := New(context.Background(), Run{WorkspaceRoot: "/host/repo", Command: "true", Image: "alpine"})

	if args := strings.Join(cmd.Args, " "); strings.Contains(args, TermsDir) {
		t.Fatalf("docker args = %q, should not mention %s when no terms folder was given", args, TermsDir)
	}
}

// TestNew_NetworkIsAlwaysDenied is the whole of issue #47's last
// requirement at this layer: there is no field, flag or config key left
// that produces a Run without --network none. Asserted over every shape
// of Run this package builds, so adding an opt-in back cannot pass by
// only touching one of them.
func TestNew_NetworkIsAlwaysDenied(t *testing.T) {
	runs := map[string]Run{
		"bare":       {WorkspaceRoot: "/host/repo", Command: "true", Image: "alpine"},
		"with user":  {WorkspaceRoot: "/host/repo", Command: "true", Image: "alpine", User: "1000:1000"},
		"with mount": {WorkspaceRoot: "/host/repo", Command: "true", Image: "alpine", SourceMounts: []Mount{{Source: "/host/repo/cmd", Target: WorkspaceDir + "/cmd"}}},
		"with terms": {WorkspaceRoot: "/host/repo", Command: "true", Image: "alpine", TermsMount: Mount{Source: "/host/terms", Target: TermsDir}},
		"with limits": {WorkspaceRoot: "/host/repo", Command: "true", Image: "alpine",
			Limits: Limits{Memory: "4g", CPUs: "1.5", PIDsLimit: 128}},
	}
	for name, run := range runs {
		cmd := New(context.Background(), run)
		if !strings.Contains(strings.Join(cmd.Args, " "), "--network none") {
			t.Fatalf("%s: docker args = %q, want --network none with no way to opt out", name, cmd.Args)
		}
	}
}

func TestNew_LimitsOverrideDefaults(t *testing.T) {
	cmd := New(context.Background(), Run{
		WorkspaceRoot: "/host/repo",
		Command:       "true",
		Image:         "alpine",
		Limits:        Limits{Memory: "4g", CPUs: "1.5", PIDsLimit: 128},
	})

	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--memory 4g", "--cpus 1.5", "--pids-limit 128"} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker args = %q, want it to contain %q", args, want)
		}
	}
}

func TestNew_UserSetsHomeAndSafeDirectory(t *testing.T) {
	cmd := New(context.Background(), Run{
		WorkspaceRoot:  "/host/repo",
		Command:        "true",
		Image:          "alpine",
		User:           "1000:1000",
		ReadOnlyMounts: []Mount{{Source: "/host/.git", Target: "/git/common"}},
	})

	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"--user 1000:1000",
		"-e HOME=/tmp",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=" + WorkspaceDir,
		"GIT_CONFIG_KEY_1=safe.directory",
		"GIT_CONFIG_VALUE_1=/git/common",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker args = %q, want it to contain %q", args, want)
		}
	}
	if strings.Contains(args, "safe.directory=*") {
		t.Fatalf("docker args = %q, want safe.directory scoped to known mounts, not widened to *", args)
	}
}

func TestNew_NoUserOmitsUserFlag(t *testing.T) {
	cmd := New(context.Background(), Run{WorkspaceRoot: "/host/repo", Command: "true", Image: "alpine"})

	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, "--user") {
		t.Fatalf("docker args = %q, should not pass --user when Run.User is empty", args)
	}
	if strings.Contains(args, "HOME=/tmp") {
		t.Fatalf("docker args = %q, should not override HOME when not running as a resolved user", args)
	}
}

func TestCmd_Started(t *testing.T) {
	cmd := New(context.Background(), Run{WorkspaceRoot: "/host/repo", Command: "true", Image: "alpine"})

	if cmd.Started() {
		t.Fatal("Started() = true before the cidfile exists, want false")
	}

	if err := os.WriteFile(cmd.cidFile, []byte("abc123\n"), 0o644); err != nil {
		t.Fatalf("writing fake cidfile: %v", err)
	}
	// Re-derive a Cmd with the same cidFile path is awkward from outside
	// the package, so exercise Started via the same instance - the
	// cidfile path never changes after New returns.
	if !cmd.Started() {
		t.Fatal("Started() = false with a populated cidfile, want true")
	}
	if _, err := os.Stat(cmd.cidFile); err != nil {
		t.Fatalf("Started() must not remove the cidfile it reads: %v", err)
	}

	cmd.Cleanup()
	if _, err := os.Stat(cmd.cidFile); !os.IsNotExist(err) {
		t.Fatal("Cleanup() should remove the cidfile")
	}
	cmd.Cleanup()
}

func TestKillContainer_AlreadyGone(t *testing.T) {
	requireDocker(t)

	err := killContainer(context.Background(), "inspector-container-test-does-not-exist")
	if !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killContainer(nonexistent) = %v, want os.ErrProcessDone", err)
	}
}

func TestKillContainer_DeadlineStopsDockerKill(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexec /bin/sleep 10\n"), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := killContainer(ctx, "stalled")
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("killContainer() = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("killContainer() returned after %s, want it bounded by the context", elapsed)
	}
}

func TestKill_CreatedButNotStartedContainerIsRemoved(t *testing.T) {
	// Issue #66's second window: the deadline can fire after Docker has
	// created the container and before it has started it, where `docker
	// kill` has no process to signal and answers "is not running". That
	// container is about to start, so reporting the refused kill and
	// leaving it is exactly the failure this issue is about - it has to
	// be removed instead. Run against real docker, because docker's own
	// behavior in that state is the thing being relied on.
	requireDocker(t)

	name := "inspector-container-test-" + randomHex(8)
	out, err := exec.Command("docker", "create", "--name", name, "alpine", "true").CombinedOutput()
	if err != nil {
		t.Skipf("could not create a container to test against: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	cmd := New(context.Background(), Run{WorkspaceRoot: t.TempDir(), Command: "true", Image: "alpine"})
	defer cmd.Cleanup()
	cmd.name = name
	// The cidfile is what the kill path resolves the container through,
	// and real docker writes it at creation - `docker create` prints the
	// same id it would have carried, last, after any platform warning.
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Skip("docker create printed no container id to test against")
	}
	id := fields[len(fields)-1]
	if err := os.WriteFile(cmd.cidFile, []byte(id+"\n"), 0o600); err != nil {
		t.Fatalf("writing the cidfile: %v", err)
	}

	if err := cmd.kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill(created-but-never-started) = %v, want the container removed rather than a refused kill", err)
	}
	if recorded := cmd.KillError(); recorded != nil {
		t.Fatalf("KillError() = %v, want nil - the container was dealt with, by removal", recorded)
	}
	if note := cmd.StopNote(); !strings.Contains(note, "not yet started") {
		t.Fatalf("StopNote() = %q, want it to say the container was removed before it could start", note)
	}
	if exec.Command("docker", "inspect", id).Run() == nil {
		t.Fatal("the created-but-never-started container is still there after kill()")
	}
}

func TestKill_RefusedKillIsAFailedKill(t *testing.T) {
	// A kill docker refuses for any reason other than "it isn't there"
	// or "it hasn't been started yet" must not be reported as handled:
	// os.ErrProcessDone, or a nil error, tells Wait the cancellation
	// succeeded, which is only safe when nothing can still be running
	// against the bind-mounted repo.
	fake := newFakeDocker(t)
	fake.env("KILL_FAILS", "1")

	cmd := NewCommand(context.Background(), "test-", "image")
	defer cmd.Cleanup()
	if err := os.WriteFile(cmd.cidFile, []byte(fakeContainerID+"\n"), 0o600); err != nil {
		t.Fatalf("writing the cidfile: %v", err)
	}

	err := cmd.kill()
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill() = %v, want a real error", err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("kill error wraps the `docker kill` process's own *exec.ExitError (%v) - callers read any ExitError as the check container's verdict", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("kill error = %q, want docker's own message", err)
	}
	if recorded := cmd.KillError(); recorded == nil || recorded.Error() != err.Error() {
		t.Fatalf("KillError() = %v, want the same failure recorded on the Cmd", recorded)
	}
	if got, want := fake.calls(), []string{"kill " + fakeContainerID}; !slices.Equal(got, want) {
		t.Fatalf("docker calls = %q, want %q - a refused kill is reported, not retried as a removal", got, want)
	}
}

func TestKill_ContainerConfirmedRunningIsAFailedKill(t *testing.T) {
	// The other side of the ruling: a container docker will not stop and
	// then confirms is running must not be reported as handled -
	// os.ErrProcessDone or a nil error tells Wait the cancellation
	// succeeded, which is only safe when nothing can still be running
	// against the bind-mounted repo.
	fake := newFakeDocker(t)
	fake.env("CID_DELAY", "never")
	fake.env("RM_FAILS", "1")
	fake.env("INSPECT_RUNNING", "true")
	containerCreateGrace = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cmd := NewCommand(ctx, "test-", "image")
	defer cmd.Cleanup()
	_ = cmd.Run()

	err := cmd.KillError()
	if err == nil {
		t.Fatal("KillError() = nil for a container docker refused to remove and then reported as running")
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("kill error wraps a cleanup process's own *exec.ExitError (%v) - callers read any ExitError as the check container's verdict", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("KillError() = %q, want docker's own message", err)
	}
	if got, want := fake.calls(), "inspect "+cmd.name; !slices.Contains(got, want) {
		t.Fatalf("docker calls = %q, want %q - only docker's own answer may decide this is a failed kill", got, want)
	}
}

// fakeDocker is a stand-in docker on PATH, modelling the timing issue #66
// is about: `docker run` writes the cidfile only after CID_DELAY seconds
// (Docker writes it at container creation, which is the slow part on a
// cold machine), and from then on behaves like a container running a
// check command - exiting 137 when something stops it, and writing the
// COMPLETED marker if nothing ever does. CID_DELAY=never models a create
// that never finishes, so no container id is ever written. Each
// subcommand it is asked for is appended to its log, with the container
// it was aimed at, so a test can tell what the kill path actually did.
type fakeDocker struct {
	t         *testing.T
	log       string
	completed string
}

const fakeContainerID = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

const fakeDockerScript = `#!/bin/sh
log() { printf '%s\n' "$*" >> "$LOG_PATH"; }
case "$1" in
run)
	cid=""
	while [ $# -gt 0 ]; do
		if [ "$1" = "--cidfile" ]; then cid="$2"; fi
		shift
	done
	log run
	if [ "$CID_DELAY" = never ]; then
		/bin/sleep "$CREATE_HANG"
		: > "$COMPLETED"
		exit 0
	fi
	/bin/sleep "$CID_DELAY"
	printf '%s' "$CONTAINER_ID" > "$cid"
	i=0
	while [ "$i" -lt 60 ]; do
		if [ -e "$STOPPED" ]; then exit 137; fi
		/bin/sleep 0.1
		i=$((i + 1))
	done
	: > "$COMPLETED"
	;;
kill)
	log "kill $2"
	if [ "$KILL_FAILS" = 1 ]; then
		echo "Error response from daemon: cannot kill container: $2: permission denied" >&2
		exit 1
	fi
	if [ -e "$STOPPED" ]; then
		echo "Error response from daemon: No such container: $2" >&2
		exit 1
	fi
	if [ "$KILL_NOT_RUNNING" = 1 ]; then
		echo "Error response from daemon: Cannot kill container: $2: Container $2 is not running" >&2
		exit 1
	fi
	: > "$STOPPED"
	;;
rm)
	log "rm $3"
	if [ "$RM_FAILS" = 1 ]; then
		echo "Error response from daemon: cannot remove container $3: permission denied" >&2
		exit 1
	fi
	if [ -e "$STOPPED" ] || [ "$CID_DELAY" = never ]; then
		echo "Error response from daemon: No such container: $3" >&2
		exit 1
	fi
	: > "$STOPPED"
	;;
inspect)
	log "inspect $4"
	printf '%s\n' "$INSPECT_RUNNING"
	;;
esac
`

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fakeDockerScript), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)

	grace := containerCreateGrace
	t.Cleanup(func() { containerCreateGrace = grace })

	f := &fakeDocker{
		t:         t,
		log:       filepath.Join(dir, "docker.log"),
		completed: filepath.Join(dir, "check-completed"),
	}
	f.env("LOG_PATH", f.log)
	f.env("COMPLETED", f.completed)
	f.env("STOPPED", filepath.Join(dir, "container-stopped"))
	f.env("CONTAINER_ID", fakeContainerID)
	f.env("CID_DELAY", "0")
	f.env("CREATE_HANG", "10")
	f.env("KILL_FAILS", "")
	f.env("KILL_NOT_RUNNING", "")
	f.env("RM_FAILS", "")
	f.env("INSPECT_RUNNING", "false")
	return f
}

func (f *fakeDocker) env(name, value string) {
	f.t.Helper()
	f.t.Setenv(name, value)
}

// calls is every docker subcommand the fake was asked for, in order, each
// with the container it named.
func (f *fakeDocker) calls() []string {
	f.t.Helper()
	data, err := os.ReadFile(f.log)
	if err != nil {
		f.t.Fatalf("reading the fake docker log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// requireCheckNeverCompleted fails the test if the modelled check command
// ran to its own end - the whole point of the timeout is that it cannot.
func (f *fakeDocker) requireCheckNeverCompleted() {
	f.t.Helper()
	if _, err := os.Stat(f.completed); !os.IsNotExist(err) {
		f.t.Fatal("the check command ran past its deadline to completion")
	}
}

func TestNewCommand_TimeoutWaitsForAContainerDockerIsStillCreating(t *testing.T) {
	// Issue #66, seen flaking on CI: the deadline fires while `docker
	// run` is still creating the container, so `docker kill` has nothing
	// to kill - and the check command then starts behind that kill and
	// runs to completion. The kill path has to wait for the container it
	// is supposed to stop.
	fake := newFakeDocker(t)
	fake.env("CID_DELAY", "0.4")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cmd := NewCommand(ctx, "test-", "image")
	defer cmd.Cleanup()
	_ = cmd.Run()

	if err := cmd.KillError(); err != nil {
		t.Fatalf("KillError() = %v, want nil - the container was waited for and stopped", err)
	}
	fake.requireCheckNeverCompleted()
	if got := fake.calls(); len(got) < 2 || got[1] != "kill "+fakeContainerID {
		t.Fatalf("docker calls = %q, want the kill aimed at the container id the cidfile only carried later", got)
	}
	if note := cmd.StopNote(); !strings.Contains(note, "waited") {
		t.Fatalf("StopNote() = %q, want it to report that stopping the check meant waiting for its container", note)
	}
}

func TestNewCommand_TimeoutRemovesAContainerCreatedButNotStarted(t *testing.T) {
	// The same race one step later: the container exists, so the cidfile
	// resolves, but it has not been started yet and `docker kill` says so.
	fake := newFakeDocker(t)
	fake.env("KILL_NOT_RUNNING", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cmd := NewCommand(ctx, "test-", "image")
	defer cmd.Cleanup()
	_ = cmd.Run()

	if err := cmd.KillError(); err != nil {
		t.Fatalf("KillError() = %v, want nil - a container that cannot be killed yet is removed instead", err)
	}
	fake.requireCheckNeverCompleted()
	if got := fake.calls(); len(got) < 3 || got[1] != "kill "+fakeContainerID || got[2] != "rm "+fakeContainerID {
		t.Fatalf("docker calls = %q, want a refused kill followed by a forced removal of the same container", got)
	}
}

func TestKill_UnremovableUnstartedContainerIsAFailedKill(t *testing.T) {
	// The third way the created-but-not-started window can end: docker
	// will neither kill that container (no process in it yet) nor remove
	// it. Nothing then stands between it and being started by the `docker
	// run` client it belongs to, so this is the one branch of that window
	// that has to be reported as a kill inspector could not carry out -
	// returning nil or os.ErrProcessDone here would tell Wait the
	// cancellation succeeded while a check command is about to start
	// against the bind-mounted repo. kill() is called directly rather than
	// through Run: a refused kill makes os/exec fall back on its own
	// WaitDelay, and waiting that out proves nothing this does not.
	fake := newFakeDocker(t)
	fake.env("KILL_NOT_RUNNING", "1")
	fake.env("RM_FAILS", "1")

	cmd := NewCommand(context.Background(), "test-", "image")
	defer cmd.Cleanup()
	if err := os.WriteFile(cmd.cidFile, []byte(fakeContainerID+"\n"), 0o600); err != nil {
		t.Fatalf("writing the cidfile: %v", err)
	}

	err := cmd.kill()
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill() = %v, want a real error for a container docker would neither kill nor remove", err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("kill error wraps a cleanup process's own *exec.ExitError (%v) - callers read any ExitError as the check container's verdict", err)
	}
	for _, want := range []string{"permission denied", "not running"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("kill error = %q, want it to carry %q - both halves of what docker refused", err, want)
		}
	}
	if recorded := cmd.KillError(); recorded == nil || recorded.Error() != err.Error() {
		t.Fatalf("KillError() = %v, want the same failure recorded on the Cmd", recorded)
	}
	if got, want := fake.calls(), []string{"kill " + fakeContainerID, "rm " + fakeContainerID}; !slices.Equal(got, want) {
		t.Fatalf("docker calls = %q, want %q - the refused kill is followed by the removal that was tried", got, want)
	}
}

func TestNewCommand_TimeoutWithNoContainerKillsTheDockerClient(t *testing.T) {
	// The last resort, per the ruling on issue #66: when no container id
	// appears within the whole wait, the `docker run` client is the only
	// handle left - killing it stops it from starting a container the
	// daemon may still be creating. A container that was never there is
	// not a failed kill, and the run says in words what it did instead.
	fake := newFakeDocker(t)
	fake.env("CID_DELAY", "never")
	containerCreateGrace = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cmd := NewCommand(ctx, "test-", "image")
	defer cmd.Cleanup()
	start := time.Now()
	_ = cmd.Run()

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run() took %s to return, want it bounded by the grace period, not waiting on a create that never finishes", elapsed)
	}
	if err := cmd.KillError(); err != nil {
		t.Fatalf("KillError() = %v, want nil - no container of this run was left running", err)
	}
	fake.requireCheckNeverCompleted()
	if note := cmd.StopNote(); !strings.Contains(note, "never reported") {
		t.Fatalf("StopNote() = %q, want it to report that Docker never produced a container to stop", note)
	}
	// With no container id to aim at, both removals fall back to this
	// run's own container name - the second of them is Wait's own last
	// catch for a container the daemon created after the kill path had
	// given up looking for one, which is the only way one can exist by
	// then.
	want := []string{"run", "rm " + cmd.name, "rm " + cmd.name}
	if got := fake.calls(); !slices.Equal(got, want) {
		t.Fatalf("docker calls = %q, want %q", got, want)
	}
}

func TestAwaitContainerID_GivesUpWhenTheDockerClientIsGone(t *testing.T) {
	// A create that failed outright - a bad image, a daemon that went
	// away - writes no cidfile and never will, so waiting out the whole
	// grace period for one would only delay the verdict.
	fake := newFakeDocker(t)
	fake.env("CID_DELAY", "never")
	fake.env("CREATE_HANG", "0.5")
	containerCreateGrace = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cmd := NewCommand(ctx, "test-", "image")
	defer cmd.Cleanup()
	start := time.Now()
	_ = cmd.Run()

	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run() took %s to return, want the wait cut short once the docker client was gone rather than run to the grace period", elapsed)
	}
	if err := cmd.KillError(); err != nil {
		t.Fatalf("KillError() = %v, want nil", err)
	}
}

func TestNew_RunsAndReportsExitCode(t *testing.T) {
	requireDocker(t)
	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := New(ctx, Run{WorkspaceRoot: dir, Command: "echo hi > out.txt; exit 7", Image: "alpine"})
	defer cmd.Cleanup()
	err := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("Run() = %v, want an *exec.ExitError with code 7", err)
	}
	if !cmd.Started() {
		t.Fatal("Started() = false for a container that ran to completion")
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil || strings.TrimSpace(string(got)) != "hi" {
		t.Fatalf("bind mount did not carry the container's write back to the host: %v, %q", err, got)
	}
}

func TestNew_BadImageIsNotStarted(t *testing.T) {
	requireDocker(t)
	dir := t.TempDir()

	cmd := New(context.Background(), Run{WorkspaceRoot: dir, Command: "true", Image: "inspector-test-image-does-not-exist-xyz"})
	defer cmd.Cleanup()
	err := cmd.Run()

	if err == nil {
		t.Fatal("expected an error running a nonexistent image")
	}
	if cmd.Started() {
		t.Fatal("Started() = true for an image docker never managed to run - the cidfile should never have been written")
	}
}

func TestNew_TimeoutKillsContainerAndDescendants(t *testing.T) {
	// Regression guard for the container replacement of check.go's old
	// process-group kill: killing the container's own init process must
	// take its background descendants with it via the kernel's PID
	// namespace teardown, not just stop the process docker run happens
	// to be attached to.
	requireDocker(t)
	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	cmd := New(ctx, Run{
		WorkspaceRoot: dir,
		Command:       "( sleep 3 && echo alive > survived ) & wait",
		Image:         "alpine",
	})
	defer cmd.Cleanup()
	start := time.Now()
	_ = cmd.Run()
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("Run() took %s to return after a 300ms timeout - the kill isn't taking effect promptly", elapsed)
	}
	if err := cmd.KillError(); err != nil {
		t.Fatalf("KillError() = %v after a kill that worked, want nil", err)
	}

	time.Sleep(3200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "survived")); !os.IsNotExist(err) {
		t.Fatal("background child survived the container being killed")
	}
}
