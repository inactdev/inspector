package inspector

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "   "}`})

	_, err := LoadConfig(dir)
	if !errors.Is(err, ErrNoCheckCommand) {
		t.Fatalf("expected ErrNoCheckCommand, got %v", err)
	}
}

func TestLoadConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check"}`})

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Check != "make check" {
		t.Fatalf("check = %q, want %q", cfg.Check, "make check")
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
	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "make check", "timeoutSeconds": 120}`})

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
