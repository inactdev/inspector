package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveUser_RejectsRemoteEndpoint(t *testing.T) {
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

	_, err := ResolveUser(context.Background())
	if !errors.Is(err, ErrRemoteDocker) {
		t.Fatalf("ResolveUser() = %v, want ErrRemoteDocker", err)
	}
}

func TestResolveUser_RejectsRemoteDockerHostOverride(t *testing.T) {
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

	_, err := ResolveUser(context.Background())
	if !errors.Is(err, ErrRemoteDocker) {
		t.Fatalf("ResolveUser() = %v, want ErrRemoteDocker", err)
	}
}

func TestResolveUser_UsesNamespaceRootForRootlessDaemon(t *testing.T) {
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

	user, err := ResolveUser(context.Background())
	if err != nil {
		t.Fatalf("ResolveUser() error = %v", err)
	}
	if user != "0:0" {
		t.Fatalf("ResolveUser() = %q, want 0:0", user)
	}
}

func TestResolveUser_UsesHostUserForRootfulLocalDaemon(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = context ]; then printf '%s\\n' '\"unix:///var/run/docker.sock\"'; exit 0; fi\n" +
		"if [ \"$1\" = info ] && [ \"${2-}\" = --format ]; then printf '%s\\n' '[]'; exit 0; fi\n" +
		"if [ \"$1\" = info ]; then exit 0; fi\n" +
		"exit 99\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")

	user, err := ResolveUser(context.Background())
	if err != nil {
		t.Fatalf("ResolveUser() error = %v", err)
	}
	if user != HostUser() {
		t.Fatalf("ResolveUser() = %q, want HostUser() (%q)", user, HostUser())
	}
}

func TestResolveUser_NotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := ResolveUser(context.Background())
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("ResolveUser() = %v, want ErrNotInstalled", err)
	}
}

func TestDockerOutput_DeadlineStopsDockerCall(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexec /bin/sleep 10\n"), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := dockerOutput(ctx, "test op", "info")
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("dockerOutput() = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("dockerOutput() returned after %s, want it bounded by the context", elapsed)
	}
}

func TestHostUser_MatchesUnixConvention(t *testing.T) {
	user := HostUser()
	if !strings.Contains(user, ":") {
		t.Fatalf("HostUser() = %q, want uid:gid", user)
	}
}
