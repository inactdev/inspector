package inspector

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

func TestLoadConfig_Missing(t *testing.T) {
	dir := t.TempDir()

	_, err := LoadConfig(dir)
	if !errors.Is(err, ErrNoCheckCommand) {
		t.Fatalf("expected ErrNoCheckCommand, got %v", err)
	}
}

func TestLoadConfig_EmptyCheck(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "   ", "image": "alpine"}`})

	_, err := LoadConfig(dir)
	if !errors.Is(err, ErrNoCheckCommand) {
		t.Fatalf("expected ErrNoCheckCommand, got %v", err)
	}
}

func TestLoadConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check", "image": "golang:1.22"}`})

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Check != "make check" {
		t.Fatalf("check = %q, want %q", cfg.Check, "make check")
	}
	if cfg.Image != "golang:1.22" {
		t.Fatalf("image = %q, want %q", cfg.Image, "golang:1.22")
	}
}

func TestLoadConfig_NoImage(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check"}`})

	_, err := LoadConfig(dir)
	if !errors.Is(err, ErrNoImage) {
		t.Fatalf("expected ErrNoImage, got %v", err)
	}
}

func TestLoadConfig_EmptyImage(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check", "image": "   "}`})

	_, err := LoadConfig(dir)
	if !errors.Is(err, ErrNoImage) {
		t.Fatalf("expected ErrNoImage, got %v", err)
	}
}

func TestLoadConfig_ReadsNetwork(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check", "image": "alpine", "network": true}`})

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Network {
		t.Fatal("Network = false, want true")
	}
}

func TestLoadConfig_NetworkDefaultsFalse(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check", "image": "alpine"}`})

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Network {
		t.Fatal("Network = true, want false (the check command must opt in)")
	}
}

func TestLoadConfig_ReadsPathLists(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check", "image": "alpine", "protectedPaths": ["script/check", "*_test.go"], "testPaths": ["*_test.go"]}`})

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"script/check", "*_test.go"}; !equalStrings(cfg.ProtectedPaths, want) {
		t.Fatalf("ProtectedPaths = %v, want %v", cfg.ProtectedPaths, want)
	}
	if want := []string{"*_test.go"}; !equalStrings(cfg.TestPaths, want) {
		t.Fatalf("TestPaths = %v, want %v", cfg.TestPaths, want)
	}
}

// Absent testPaths has to stay absent rather than become a default: an empty
// pattern list is what makes inspector judge nothing and post no
// inspector-tests status, and a guessed default would clear protected test
// files in a project that never asked for the judgment.
func TestLoadConfig_PathListsDefaultToNone(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check", "image": "alpine"}`})

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ProtectedPaths) != 0 {
		t.Fatalf("ProtectedPaths = %v, want none", cfg.ProtectedPaths)
	}
	if len(cfg.TestPaths) != 0 {
		t.Fatalf("TestPaths = %v, want none", cfg.TestPaths)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestConfig_Limits_Defaults(t *testing.T) {
	cfg := &Config{Check: "true"}
	got := cfg.Limits()
	if got.Memory != "" || got.CPUs != "" || got.PIDsLimit != 0 {
		t.Fatalf("Limits() = %+v, want the zero value so container.New applies its own defaults", got)
	}
}

func TestLoadConfig_ReadsLimits(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check", "image": "alpine", "memory": "4g", "cpus": "1", "pidsLimit": 64}`})

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := container.Limits{Memory: "4g", CPUs: "1", PIDsLimit: 64}
	if got := cfg.Limits(); got != want {
		t.Fatalf("Limits() = %+v, want %+v", got, want)
	}
}

func TestLoadConfig_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{not json`})

	_, err := LoadConfig(dir)
	if err == nil || errors.Is(err, ErrNoCheckCommand) {
		t.Fatalf("expected a parse error, got %v", err)
	}
}

func TestConfig_Timeout_Default(t *testing.T) {
	cfg := &Config{Check: "true"}
	if got := cfg.Timeout(); got != DefaultTimeout {
		t.Fatalf("Timeout() = %s, want DefaultTimeout (%s)", got, DefaultTimeout)
	}
}

func TestConfig_Timeout_Configured(t *testing.T) {
	cfg := &Config{Check: "true", TimeoutSeconds: 90}
	if got, want := cfg.Timeout(), 90*time.Second; got != want {
		t.Fatalf("Timeout() = %s, want %s", got, want)
	}
}

func TestLoadConfig_ReadsTimeoutSeconds(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check", "image": "alpine", "timeoutSeconds": 120}`})

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := cfg.Timeout(), 120*time.Second; got != want {
		t.Fatalf("Timeout() = %s, want %s", got, want)
	}
}

func TestLoadConfig_UnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(`{"check": "x"}`), 0o000); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })

	if os.Geteuid() == 0 {
		t.Skip("running as root, permission bits are not enforced")
	}

	_, err := LoadConfig(dir)
	if err == nil || errors.Is(err, ErrNoCheckCommand) {
		t.Fatalf("expected a read error, got %v", err)
	}
}
