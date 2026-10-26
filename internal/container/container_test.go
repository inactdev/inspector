package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func TestKillContainer_NotRunningIsARealError(t *testing.T) {
	// A container docker refuses to kill for any reason other than "it
	// isn't there" must not be reported as handled: os.ErrProcessDone
	// tells Wait the cancellation succeeded, which is only safe when
	// nothing can still be running.
	requireDocker(t)

	name := "inspector-container-test-" + randomHex(8)
	if out, err := exec.Command("docker", "create", "--name", name, "alpine", "true").CombinedOutput(); err != nil {
		t.Skipf("could not create a container to test against: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	cmd := New(context.Background(), Run{WorkspaceRoot: t.TempDir(), Command: "true", Image: "alpine"})
	defer cmd.Cleanup()
	cmd.name = name

	err := cmd.kill()
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill(created-but-never-started) = %v, want a real error", err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("kill error wraps the `docker kill` process's own *exec.ExitError (%v) - callers read any ExitError as the check container's verdict", err)
	}
	if !strings.Contains(err.Error(), "is not running") {
		t.Fatalf("kill error = %q, want docker's own message", err)
	}
	if recorded := cmd.KillError(); recorded == nil || recorded.Error() != err.Error() {
		t.Fatalf("KillError() = %v, want the same failure recorded on the Cmd", recorded)
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

func TestNewCommand_TimeoutCleansUpContainerCreatedAfterInitialKill(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	marker := filepath.Join(dir, "container-exists")
	logPath := filepath.Join(dir, "docker.log")
	script := `#!/bin/sh
case "$1" in
run)
  /bin/sleep 0.15
  : > "$MARKER"
  ;;
kill)
  printf 'kill\n' >> "$LOG_PATH"
  if [ -e "$MARKER" ]; then
    /bin/rm -f "$MARKER"
    exit 0
  fi
  echo 'No such container' >&2
  exit 1
  ;;
rm)
  printf 'rm\n' >> "$LOG_PATH"
  /bin/rm -f "$MARKER"
  ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("MARKER", marker)
	t.Setenv("LOG_PATH", logPath)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	cmd := NewCommand(ctx, "test-", "image")
	defer cmd.Cleanup()
	_ = cmd.Run()
	if err := cmd.KillError(); err != nil {
		t.Fatalf("KillError() = %v, want cleanup after docker run exited", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("late-created container marker still exists: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading fake docker log: %v", err)
	}
	if got := strings.Fields(string(logData)); len(got) != 2 || got[0] != "kill" || got[1] != "rm" {
		t.Fatalf("docker cleanup calls = %q, want initial kill followed by confirmed removal", logData)
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
