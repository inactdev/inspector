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
)

// requireDocker skips a test that genuinely needs a live container
// runtime, rather than failing on a machine without one - the same
// tolerance LoadConfig's own permission-bit test already extends to a
// root-run suite (internal/inspector/config_test.go).
func requireDocker(t *testing.T) {
	t.Helper()
	if err := EnsureAvailable(); err != nil {
		t.Skipf("no usable container runtime, skipping: %v", err)
	}
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

func TestNew_BuildsExpectedArgs(t *testing.T) {
	// Pure argument construction - doesn't touch docker, so it runs
	// without a runtime present.
	cmd := New(context.Background(), Run{
		RepoRoot: "/host/repo",
		Command:  "go test ./...",
		Image:    "golang:1.22",
	})

	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"docker run --rm",
		"-v /host/repo:" + WorkspaceDir,
		"-w " + WorkspaceDir,
		"--network none",
		"golang:1.22 sh -c go test ./...",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker args = %q, want it to contain %q", args, want)
		}
	}
}

func TestNew_NetworkOptsIn(t *testing.T) {
	cmd := New(context.Background(), Run{RepoRoot: "/host/repo", Command: "true", Image: "alpine", Network: true})

	if strings.Contains(strings.Join(cmd.Args, " "), "--network none") {
		t.Fatalf("docker args = %q, should not restrict network when Network is true", cmd.Args)
	}
}

func TestCmd_Started(t *testing.T) {
	cmd := New(context.Background(), Run{RepoRoot: "/host/repo", Command: "true", Image: "alpine"})

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

	err := killContainer("inspector-container-test-does-not-exist")
	if !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killContainer(nonexistent) = %v, want os.ErrProcessDone", err)
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

	cmd := New(context.Background(), Run{RepoRoot: t.TempDir(), Command: "true", Image: "alpine"})
	defer cmd.Cleanup()

	err := cmd.kill(name)
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

	cmd := New(ctx, Run{RepoRoot: dir, Command: "echo hi > out.txt; exit 7", Image: "alpine"})
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

	cmd := New(context.Background(), Run{RepoRoot: dir, Command: "true", Image: "inspector-test-image-does-not-exist-xyz"})
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
		RepoRoot: dir,
		Command:  "( sleep 3 && echo alive > survived ) & wait",
		Image:    "alpine",
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
