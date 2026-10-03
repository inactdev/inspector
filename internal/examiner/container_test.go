package examiner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

func TestBundledAgentRunsOnDockerServerPlatform(t *testing.T) {
	if err := container.EnsureAvailable(); err != nil {
		t.Skipf("no usable container runtime, skipping: %v", err)
	}
	agentExecutable, cleanup, err := prepareAgentExecutable(context.Background())
	if err != nil {
		t.Fatalf("prepareAgentExecutable() error = %v", err)
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--read-only", "--network", "none",
		"--mount", "type=bind,src="+agentExecutable+",dst=/examiner/agent,readonly",
		"--entrypoint", "/examiner/agent", RuntimeImage, "--help").CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 64 {
		t.Fatalf("bundled examiner agent --help error = %v, output = %s", err, output)
	}
	if !strings.Contains(string(output), "Usage of examiner-agent") {
		t.Fatalf("bundled examiner agent did not emit its CLI help: %s", output)
	}
}

func TestDockerServerTarget_DeadlineStopsPlatformProbe(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexec /bin/sleep 10\n"), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := dockerServerTarget(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dockerServerTarget() = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("dockerServerTarget() returned after %s, want it bounded by the context", elapsed)
	}
}

func TestNewContainerCommand_ApplicationSourceCannotBeMounted(t *testing.T) {
	applicationSource := t.TempDir()
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	agentExecutable := filepath.Join(t.TempDir(), "examiner-agent")
	if err := os.WriteFile(agentExecutable, []byte("placeholder"), 0o700); err != nil {
		t.Fatalf("writing agent placeholder: %v", err)
	}
	cmd := NewContainerCommand(context.Background(), ContainerOptions{
		InputDir: inputDir, OutputDir: outputDir, AgentExecutable: agentExecutable,
		AppURL: "http://app:8080", Model: "test-model", Network: "examiner-test", Timeout: 23 * time.Minute,
	})
	defer cmd.Cleanup()

	mounts := dockerMountSources(t, cmd.Args)
	if mounts[applicationSource] {
		t.Fatalf("application source %q is reachable from the examiner container", applicationSource)
	}
	want := map[string]bool{inputDir: true, outputDir: true, agentExecutable: true}
	if len(mounts) != len(want) {
		t.Fatalf("container mount sources = %#v, want only %#v", mounts, want)
	}
	for source := range want {
		if !mounts[source] {
			t.Fatalf("container does not mount expected examiner resource %q: %#v", source, mounts)
		}
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--read-only") {
		t.Fatalf("docker command must keep the examiner filesystem read-only: %q", cmd.Args)
	}
	if !strings.Contains(args, "--entrypoint /examiner/agent "+RuntimeImage) {
		t.Fatalf("docker command must force Inspector as the entrypoint in the pinned runtime: %q", cmd.Args)
	}
	if !strings.Contains(args, "--timeout 23m0s") {
		t.Fatalf("docker command must forward the examination timeout to the agent: %q", cmd.Args)
	}
}

func TestPreparedVerdictRemainsReadableAfterContainerWrite(t *testing.T) {
	if err := container.EnsureAvailable(); err != nil {
		t.Skipf("no usable container runtime, skipping: %v", err)
	}
	outputDir, verdictPath, cleanupOutput, err := prepareOutput()
	if err != nil {
		t.Fatalf("prepareOutput() error = %v", err)
	}
	defer cleanupOutput()
	if _, err := os.Stat(verdictPath); err != nil {
		t.Fatalf("verdict was not pre-created on the host: %v", err)
	}

	agentExecutable := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(agentExecutable, []byte("#!/bin/sh\nprintf container-verdict > /examiner/output/verdict.json\n"), 0o700); err != nil {
		t.Fatalf("writing agent: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := NewContainerCommand(ctx, ContainerOptions{
		InputDir: t.TempDir(), OutputDir: outputDir, AgentExecutable: agentExecutable,
		AppURL: "http://app:8080", Model: "test-model", Network: "none",
	})
	defer cmd.Cleanup()
	if err := cmd.Run(); err != nil {
		t.Fatalf("container write failed: %v", err)
	}
	data, err := os.ReadFile(verdictPath)
	if err != nil {
		t.Fatalf("host could not read container-written verdict: %v", err)
	}
	if string(data) != "container-verdict" {
		t.Fatalf("verdict = %q, want container-verdict", data)
	}
}

func TestSealedContainerMountsExcludeApplicationSource(t *testing.T) {
	if err := container.EnsureAvailable(); err != nil {
		t.Skipf("no usable container runtime, skipping: %v", err)
	}
	applicationSource := t.TempDir()
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	agentExecutable := filepath.Join(t.TempDir(), "examiner-agent")
	if err := os.WriteFile(agentExecutable, []byte("placeholder"), 0o700); err != nil {
		t.Fatalf("writing agent placeholder: %v", err)
	}
	cmd := NewContainerCommand(context.Background(), ContainerOptions{
		InputDir: inputDir, OutputDir: outputDir, AgentExecutable: agentExecutable,
		AppURL: "http://app:8080", Model: "test-model", Network: "none",
	})
	defer cmd.Cleanup()
	name := fmt.Sprintf("inspector-examiner-mount-test-%d", time.Now().UnixNano())
	args := []string{"create", "--name", name}
	for n := 2; n < len(cmd.Args); n++ {
		switch cmd.Args[n] {
		case "--rm":
		case "--name", "--cidfile":
			n++
		default:
			args = append(args, cmd.Args[n])
		}
	}
	if output, err := runDocker(args...); err != nil {
		t.Skipf("could not create alpine examiner container: %v: %s", err, output)
	}
	t.Cleanup(func() { _, _ = runDocker("rm", "-f", name) })

	mountOutput, err := runDocker("inspect", "--format", "{{range .Mounts}}{{.Source}}\n{{end}}", name)
	if err != nil {
		t.Fatalf("inspecting sealed examiner mounts: %v: %s", err, mountOutput)
	}
	for _, source := range strings.Fields(mountOutput) {
		if source == applicationSource {
			t.Fatalf("Docker mounted application source %q into the sealed examiner", applicationSource)
		}
	}
	if strings.Contains(mountOutput, applicationSource) {
		t.Fatalf("Docker mount configuration leaks application source %q: %s", applicationSource, mountOutput)
	}
}

func TestNewContainerCommand_TimeoutKillsContainerAndDescendants(t *testing.T) {
	if err := container.EnsureAvailable(); err != nil {
		t.Skipf("no usable container runtime, skipping: %v", err)
	}
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	agentExecutable := filepath.Join(t.TempDir(), "agent")
	script := "#!/bin/sh\n(sleep 2; echo survived > /examiner/output/survived) &\nwait\n"
	if err := os.WriteFile(agentExecutable, []byte(script), 0o700); err != nil {
		t.Fatalf("writing agent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := NewContainerCommand(ctx, ContainerOptions{
		InputDir: inputDir, OutputDir: outputDir, AgentExecutable: agentExecutable,
		AppURL: "http://app:8080", Model: "test-model", Network: "none",
	})
	defer cmd.Cleanup()
	start := time.Now()
	_ = cmd.Run()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("container returned %s after its timeout", elapsed)
	}
	if err := cmd.KillError(); err != nil {
		t.Fatalf("stopping timed-out examiner container: %v", err)
	}

	time.Sleep(2200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(outputDir, "survived")); !os.IsNotExist(err) {
		t.Fatal("examiner child survived the timed-out container")
	}
}

func runDocker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

func dockerMountSources(t *testing.T, args []string) map[string]bool {
	t.Helper()
	mounts := map[string]bool{}
	for n, arg := range args {
		if arg != "--mount" || n+1 == len(args) {
			continue
		}
		for _, part := range strings.Split(args[n+1], ",") {
			if source, ok := strings.CutPrefix(part, "src="); ok {
				mounts[source] = true
			}
		}
	}
	return mounts
}
