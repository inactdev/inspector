package examiner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testRuntimeImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNewContainerCommand_OnlyMountsAllowedResources(t *testing.T) {
	applicationSource := t.TempDir()
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	cmd := newContainerCommand(context.Background(), containerOptions{
		InputDir: inputDir, OutputDir: outputDir, AppURL: "http://app:8080", Model: "test-model", Network: "examiner-test", Timeout: 23 * time.Minute, Budget: 7,
	}, testRuntimeImageID)
	defer cmd.Cleanup()

	mounts := dockerMounts(t, cmd.Args)
	want := map[string]dockerMount{
		"/examiner/input":  {source: inputDir, readOnly: true},
		"/examiner/output": {source: outputDir},
	}
	if len(mounts) != len(want) {
		t.Fatalf("container mounts = %#v, want exactly %#v", mounts, want)
	}
	for target, expected := range want {
		actual, ok := mounts[target]
		if !ok {
			t.Fatalf("container omitted required mount %q: %#v", target, mounts)
		}
		if actual != expected {
			t.Fatalf("container mount %q = %#v, want %#v", target, actual, expected)
		}
	}
	for _, mount := range mounts {
		if pathMakesReachable(mount.source, applicationSource) {
			t.Fatalf("examiner mount %q makes application source %q reachable", mount.source, applicationSource)
		}
	}
	if strings.Contains(strings.Join(cmd.Args, "\x00"), "examiner-agent-linux") {
		t.Fatalf("container command must not mount caller or committed agent executables: %q", cmd.Args)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--pull never") {
		t.Fatalf("examination must never pull a runtime image: %q", cmd.Args)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--entrypoint /usr/local/bin/examiner-agent "+testRuntimeImageID) {
		t.Fatalf("container command must force the verified immutable runtime entrypoint: %q", cmd.Args)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--timeout 23m0s --budget 7") {
		t.Fatalf("container command must forward timeout and budget: %q", cmd.Args)
	}
}

func TestNewContainerCommand_RejectsEveryOtherMountSpelling(t *testing.T) {
	cmd := newContainerCommand(context.Background(), containerOptions{
		InputDir: "/private/input", OutputDir: "/private/output", AppURL: "http://app:8080", Model: "test-model", Network: "none",
	}, testRuntimeImageID)
	defer cmd.Cleanup()
	for n, arg := range cmd.Args {
		if arg == "--mount" {
			if n+1 == len(cmd.Args) {
				t.Fatal("--mount has no value")
			}
			continue
		}
		if strings.HasPrefix(arg, "-v") || strings.HasPrefix(arg, "--volume") || strings.HasPrefix(arg, "--mount=") {
			t.Fatalf("container command used unsupported mount spelling %q: %q", arg, cmd.Args)
		}
	}
}

func TestRuntimeContainerMountsExactlyAllowedResources(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	user, err := localDockerRuntimeUser(ctx)
	if err != nil {
		t.Skipf("no usable local Docker runtime, skipping: %v", err)
	}
	runtimeImage, err := ensureRuntimeImage(ctx, user)
	if err != nil {
		t.Skipf("local examiner runtime is unavailable, skipping: %v", err)
	}
	applicationSource, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving application source fixture: %v", err)
	}
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	cmd := newContainerCommand(ctx, containerOptions{
		InputDir: inputDir, OutputDir: outputDir, AppURL: "http://app:8080", Model: "test-model", Network: "none", User: user,
	}, runtimeImage)
	defer cmd.Cleanup()
	name := fmt.Sprintf("inspector-examiner-mount-test-%d", time.Now().UnixNano())
	args := dockerCreateArgs(cmd.Args, name)
	if output, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("creating sealed examiner container: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	output, err := exec.Command("docker", "inspect", "--format", "{{json .Mounts}}", name).Output()
	if err != nil {
		t.Fatalf("inspecting sealed examiner mounts: %v", err)
	}
	var mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	}
	if err := json.Unmarshal(output, &mounts); err != nil {
		t.Fatalf("decoding sealed examiner mounts: %v: %s", err, output)
	}
	want := map[string]dockerMount{
		"/examiner/input":  {source: inputDir, readOnly: true},
		"/examiner/output": {source: outputDir},
	}
	if len(mounts) != len(want) {
		t.Fatalf("runtime mounts = %#v, want exactly %#v", mounts, want)
	}
	for _, mount := range mounts {
		expected, ok := want[mount.Destination]
		if !ok {
			t.Fatalf("runtime exposes unexpected mount %#v", mount)
		}
		source := normalizeDockerMountSource(mount.Source)
		if mount.Type != "bind" || source != normalizeDockerMountSource(expected.source) || mount.RW != !expected.readOnly {
			t.Fatalf("runtime mount %#v does not match expected %#v", mount, expected)
		}
		if pathMakesReachable(source, normalizeDockerMountSource(applicationSource)) {
			t.Fatalf("runtime mount %q makes application source %q reachable", source, applicationSource)
		}
		delete(want, mount.Destination)
	}
	if len(want) != 0 {
		t.Fatalf("runtime omitted mounts: %#v", want)
	}
}

func dockerCreateArgs(command []string, name string) []string {
	args := []string{"create", "--name", name}
	for n := 2; n < len(command); n++ {
		switch command[n] {
		case "--rm":
		case "--name", "--cidfile":
			n++
		default:
			args = append(args, command[n])
		}
	}
	return args
}

func normalizeDockerMountSource(path string) string {
	for _, prefix := range []string{"/host_mnt", "/run/desktop/mnt/host"} {
		if strings.HasPrefix(path, prefix+"/") {
			path = strings.TrimPrefix(path, prefix)
			break
		}
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func TestEnsureRuntimeImage_RejectsStaleAgent(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = image ]; then printf '%s\\n' '" + testRuntimeImageID + "'; exit 0; fi\n" +
		"if [ \"$1\" = run ]; then printf '%s\\n' stale-fingerprint; exit 0; fi\n" +
		"exit 99\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)

	_, err := ensureRuntimeImage(context.Background(), "123:456")
	if err == nil || !strings.Contains(err.Error(), "stale or incompatible") || !strings.Contains(err.Error(), "build.sh") {
		t.Fatalf("ensureRuntimeImage() error = %v, want stale image rebuild refusal", err)
	}
}

func TestEnsureRuntimeImagePinsVerifiedImageID(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = image ]; then printf '%s\\n' '" + testRuntimeImageID + "'; exit 0; fi\n" +
		"if [ \"$1\" = run ]; then\n" +
		"  case \" $* \" in *\" " + testRuntimeImageID + " --runtime-fingerprint \"*) printf '%s\\n' '" + RuntimeSourceFingerprint() + "'; exit 0;; esac\n" +
		"fi\n" +
		"exit 99\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)

	image, err := ensureRuntimeImage(context.Background(), "123:456")
	if err != nil {
		t.Fatalf("ensureRuntimeImage() error = %v", err)
	}
	if image != testRuntimeImageID {
		t.Fatalf("ensureRuntimeImage() = %q, want %q", image, testRuntimeImageID)
	}
}

func TestRuntimeIdentityCommand_CancellationCleansUpContainer(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	marker := filepath.Join(dir, "identity-container-exists")
	logPath := filepath.Join(dir, "docker.log")
	script := `#!/bin/sh
case "$1" in
run)
  shift
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "--cidfile" ]; then
      printf 'identity-container\n' > "$2"
      shift 2
      continue
    fi
    shift
  done
  : > "$MARKER"
  while [ -e "$MARKER" ]; do /bin/sleep 0.01; done
  ;;
kill)
  printf 'kill\n' >> "$LOG_PATH"
  /bin/rm -f "$MARKER"
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
	ctx, cancel := context.WithCancel(context.Background())
	cmd := newRuntimeIdentityCommand(ctx, "123:456", testRuntimeImageID)
	defer cmd.Cleanup()
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()

	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("identity container did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("identity container did not stop after cancellation")
	}
	if err := cmd.KillError(); err != nil {
		t.Fatalf("KillError() = %v, want successful cleanup", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("identity container marker still exists: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading fake docker log: %v", err)
	}
	if got := strings.Fields(string(logData)); len(got) != 2 || got[0] != "kill" || got[1] != "rm" {
		t.Fatalf("docker cleanup calls = %q, want kill followed by confirmed removal", logData)
	}
}

func TestLocalDockerRuntimeUser_RejectsRemoteEndpoint(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = context ]; then printf '%s\\n' '\"ssh://builder.example/run/docker.sock\"'; exit 0; fi\n" +
		"exit 99\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")

	_, err := localDockerRuntimeUser(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remote Docker endpoint") {
		t.Fatalf("localDockerRuntimeUser() = %v, want remote endpoint refusal", err)
	}
}

func TestLocalDockerRuntimeUser_RejectsRemoteDockerHostOverride(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = context ]; then printf '%s\\n' '\"unix:///var/run/docker.sock\"'; exit 0; fi\n" +
		"if [ \"$1\" = info ]; then exit 0; fi\n" +
		"exit 99\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "ssh://builder.example/run/docker.sock")

	_, err := localDockerRuntimeUser(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remote Docker endpoint") {
		t.Fatalf("localDockerRuntimeUser() = %v, want DOCKER_HOST refusal", err)
	}
}

func TestLocalDockerRuntimeUser_UsesNamespaceRootForRootlessDaemon(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = context ]; then printf '%s\\n' '\"unix:///run/user/1000/docker.sock\"'; exit 0; fi\n" +
		"if [ \"$1\" = info ] && [ \"${2-}\" = --format ]; then printf '%s\\n' '[\"name=rootless\"]'; exit 0; fi\n" +
		"if [ \"$1\" = info ]; then exit 0; fi\n" +
		"exit 99\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")

	user, err := localDockerRuntimeUser(context.Background())
	if err != nil {
		t.Fatalf("localDockerRuntimeUser() error = %v", err)
	}
	if user != "0:0" {
		t.Fatalf("localDockerRuntimeUser() = %q, want 0:0", user)
	}
}

type dockerMount struct {
	source   string
	readOnly bool
}

func dockerMounts(t *testing.T, args []string) map[string]dockerMount {
	t.Helper()
	mounts := map[string]dockerMount{}
	for n, arg := range args {
		if arg != "--mount" {
			continue
		}
		if n+1 == len(args) {
			t.Fatal("--mount has no value")
		}
		mount := dockerMount{}
		var target string
		for _, part := range strings.Split(args[n+1], ",") {
			switch {
			case strings.HasPrefix(part, "src="):
				mount.source = strings.TrimPrefix(part, "src=")
			case strings.HasPrefix(part, "dst="):
				target = strings.TrimPrefix(part, "dst=")
			case part == "readonly":
				mount.readOnly = true
			}
		}
		if mount.source == "" || target == "" {
			t.Fatalf("invalid mount %q", args[n+1])
		}
		if _, exists := mounts[target]; exists {
			t.Fatalf("duplicate mount target %q", target)
		}
		mounts[target] = mount
	}
	return mounts
}

func pathMakesReachable(source, target string) bool {
	relative, err := filepath.Rel(source, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
