package alwaystrue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

// runningApp is the app check 3 brought up for one run.
//
// Two URLs, because two kinds of falsifier reach the app by different routes.
// HostURL is the ephemeral loopback port Docker published, which is how
// Inspector itself performs an http falsifier from the host. ContainerURL is
// the app's own port, which is what a command or script falsifier uses: those
// run in a container joined with `--network container:<app>`, sharing the app
// container's network stack, so for them the app is on 127.0.0.1 at the port
// it actually listens on. Docker does not accept `--publish` on a container in
// that mode - publishing on the app container covers the whole group.
type runningApp struct {
	Name         string
	HostURL      string
	ContainerURL string
}

// startApp brings the app up from the image the list names, with its own
// fixture data, and returns a teardown that always removes both. It never
// builds or pulls: `--pull never` means a missing image is refused by name
// before anything starts, the same stance the examiner's runtime takes.
func startApp(ctx context.Context, app *App, user string, limits container.Limits) (*runningApp, func(), error) {
	fixtureDir, err := writeFixture(app.Fixture)
	if err != nil {
		return nil, func() {}, err
	}
	name := "inspector-always-app-" + randomHex(8)
	teardown := func() {
		removeContainer(name)
		if fixtureDir != "" {
			_ = os.RemoveAll(fixtureDir)
		}
	}

	args := []string{"run", "--detach", "--name", name}
	args = append(args, isolationArgs(user, limits)...)
	args = append(args, "--publish", fmt.Sprintf("127.0.0.1:0:%d", app.Port))
	if fixtureDir != "" {
		args = append(args, "-v", fixtureDir+":"+app.Fixture.Dir)
	}
	// Values, not names: this list is a committed, reviewed file, so it
	// carries configuration the project is happy to publish and never a
	// secret. A secret the app needs is the reason issue #47 wants projects
	// committing their own image, not something to add a passthrough for.
	for _, variable := range sortedKeys(app.Env) {
		args = append(args, "--env", variable+"="+app.Env[variable])
	}
	args = append(args, app.Image)
	args = append(args, app.Args...)

	if output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		teardown()
		return nil, func() {}, fmt.Errorf("the app would not start: starting %s: %s", app.Image, dockerMessage(output, err))
	}

	hostPort, err := publishedPort(ctx, name, app.Port)
	if err != nil {
		teardown()
		return nil, func() {}, fmt.Errorf("the app would not start: %w", err)
	}
	running := &runningApp{
		Name:         name,
		HostURL:      "http://127.0.0.1:" + hostPort,
		ContainerURL: fmt.Sprintf("http://127.0.0.1:%d", app.Port),
	}
	if err := waitForReady(ctx, running, app.Ready); err != nil {
		message := err.Error()
		if logs := containerLogs(name); logs != "" {
			message += "; its last output was: " + logs
		}
		teardown()
		return nil, func() {}, fmt.Errorf("the app would not start: %s", message)
	}
	return running, teardown, nil
}

// isolationArgs is the containment both the app container and every falsifier
// container gets. It is the examiner's runtimeIsolationArgs with the one
// difference the app needs: a writable fixture mount, added by the caller.
// Deliberately no `--userns host`, for the same reason the examiner gives: a
// host whose administrator configured userns-remap keeps that isolation.
func isolationArgs(user string, limits container.Limits) []string {
	memory := limits.Memory
	if memory == "" {
		memory = container.DefaultMemory
	}
	cpus := limits.CPUs
	if cpus == "" {
		cpus = container.DefaultCPUs
	}
	pids := limits.PIDsLimit
	if pids == 0 {
		pids = container.DefaultPIDsLimit
	}
	args := []string{
		"--pull", "never",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--memory", memory,
		"--cpus", cpus,
		"--pids-limit", strconv.Itoa(pids),
	}
	if user == "" {
		user = container.HostUser()
	}
	// HOME for the same reason container.New sets it: many images default it
	// to a directory only their own default user can write, and this runs as
	// the inviting host uid instead.
	return append(args, "--user", user, "--env", "HOME=/tmp")
}

// writeFixture creates the throwaway data directory the app gets, so every run
// drives a known starting state rather than whatever an earlier run left.
func writeFixture(fixture *Fixture) (string, error) {
	if fixture == nil {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "inspector-always-fixture-")
	if err != nil {
		return "", fmt.Errorf("creating the fixture directory: %w", err)
	}
	for _, name := range sortedKeys(fixture.Files) {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("creating fixture directory for %s: %w", name, err)
		}
		if err := os.WriteFile(target, []byte(fixture.Files[name]), 0o644); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("writing fixture file %s: %w", name, err)
		}
	}
	return dir, nil
}

// publishedPort reads back the ephemeral host port Docker chose. `docker port`
// answers either "127.0.0.1:49155" or "8080/tcp -> 127.0.0.1:49155" depending
// on how it is called and on the host's address families, so only the trailing
// port number is read and the host is always loopback - which is what the
// publish above bound it to.
func publishedPort(ctx context.Context, name string, port int) (string, error) {
	spec := fmt.Sprintf("%d/tcp", port)
	output, err := exec.CommandContext(ctx, "docker", "port", name, spec).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("reading the published port for %s: %s", spec, dockerMessage(output, err))
	}
	hostPort, err := parsePublishedPort(string(output))
	if err != nil {
		return "", fmt.Errorf("reading the published port for %s: %w", spec, err)
	}
	return hostPort, nil
}

// parsePublishedPort takes only the trailing port number, from whichever of
// the two forms docker answered with, and ignores the address: the publish
// above bound it to loopback, which is where it is dialed regardless. A
// dual-stack host can list more than one mapping, so the first usable one
// wins rather than the parse failing.
func parsePublishedPort(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		candidate := line[strings.LastIndex(line, ":")+1:]
		if number, err := strconv.Atoi(candidate); err == nil && number > 0 && number < 65536 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("docker reported no usable host port: %q", strings.TrimSpace(output))
}

// waitForReady refuses to drive a single promise until the app has answered
// its own declared readiness request. Without it, a promise could pass or fail
// against an app that was not listening yet, which is a result about timing
// rather than about the product.
func waitForReady(ctx context.Context, app *runningApp, ready Ready) error {
	deadline := time.Now().Add(ready.ReadyTimeout())
	var lastDetail string
	nextRunningCheck := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for %s to answer %s: %w", app.HostURL, ready.Path, err)
		}
		status, _, err := doRequest(ctx, app.HostURL, HTTPStep{Method: http.MethodGet, Path: ready.Path})
		switch {
		case err != nil:
			lastDetail = err.Error()
		case status == ready.Status:
			return nil
		default:
			lastDetail = fmt.Sprintf("answered %d, not the expected %d", status, ready.Status)
		}
		if time.Now().After(nextRunningCheck) {
			nextRunningCheck = time.Now().Add(2 * time.Second)
			if running, checkErr := containerRunning(ctx, app.Name); checkErr == nil && !running {
				return fmt.Errorf("the app container exited before answering %s", ready.Path)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("it did not answer %s with %d within %s: %s", ready.Path, ready.Status, ready.ReadyTimeout(), lastDetail)
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func containerRunning(ctx context.Context, name string) (bool, error) {
	output, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", name).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("inspecting %s: %s", name, dockerMessage(output, err))
	}
	return strings.TrimSpace(string(output)) == "true", nil
}

// containerLogs is diagnosis only, and is why the app container is not run
// with `--rm`: an app that exited on its own would otherwise be gone before
// anyone could be told why it would not start.
func containerLogs(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "logs", "--tail", "20", name).CombinedOutput()
	if err != nil {
		return ""
	}
	return truncate(strings.TrimSpace(string(output)), 600)
}

// removeContainer runs on its own context, because teardown has to happen
// even when the run's own context is already cancelled or past its deadline -
// a leaked app container would hold the published port and the fixture mount.
func removeContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "rm", "--force", "--volumes", name).Run()
}

func dockerMessage(output []byte, err error) string {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return err.Error()
	}
	return truncate(message, 600)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(buf)
}

// sortedKeys keeps the docker argument list and the fixture write order the
// same for the same list, so a failure is reproducible from what was recorded.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
