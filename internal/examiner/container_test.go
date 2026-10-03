package examiner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

func TestNewContainerCommand_ApplicationSourceCannotBeMounted(t *testing.T) {
	applicationSource := t.TempDir()
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	executable := filepath.Join(t.TempDir(), "inspector")
	if err := os.WriteFile(executable, []byte("placeholder"), 0o700); err != nil {
		t.Fatalf("writing executable placeholder: %v", err)
	}
	cmd := NewContainerCommand(context.Background(), ContainerOptions{
		InputDir: inputDir, OutputDir: outputDir, Executable: executable,
		AppURL: "http://app:8080", Model: "test-model", Image: "alpine:3.20", Network: "examiner-test",
	})

	mounts := dockerMountSources(t, cmd.Args)
	if mounts[applicationSource] {
		t.Fatalf("application source %q is reachable from the examiner container", applicationSource)
	}
	want := map[string]bool{inputDir: true, outputDir: true, executable: true}
	if len(mounts) != len(want) {
		t.Fatalf("container mount sources = %#v, want only %#v", mounts, want)
	}
	for source := range want {
		if !mounts[source] {
			t.Fatalf("container does not mount expected examiner resource %q: %#v", source, mounts)
		}
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--read-only") {
		t.Fatalf("docker command must keep the examiner filesystem read-only: %q", cmd.Args)
	}
}

func TestSealedContainerMountsExcludeApplicationSource(t *testing.T) {
	if err := container.EnsureAvailable(); err != nil {
		t.Skipf("no usable container runtime, skipping: %v", err)
	}
	applicationSource := t.TempDir()
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	executable := filepath.Join(t.TempDir(), "inspector")
	if err := os.WriteFile(executable, []byte("placeholder"), 0o700); err != nil {
		t.Fatalf("writing executable placeholder: %v", err)
	}
	cmd := NewContainerCommand(context.Background(), ContainerOptions{
		InputDir: inputDir, OutputDir: outputDir, Executable: executable,
		AppURL: "http://app:8080", Model: "test-model", Image: "alpine:3.20", Network: "none",
	})
	args := append([]string(nil), cmd.Args[1:]...)
	args[0] = "create"
	name := fmt.Sprintf("inspector-examiner-mount-test-%d", time.Now().UnixNano())
	args = append([]string{"create", "--name", name}, args[1:]...)
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
