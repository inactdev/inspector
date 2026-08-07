package inspector

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ConfigFileName is the project-level config inspector reads to learn the
// project's own definition of green. It is committed to the repo, not
// gitignored, so it is visible for review and eligible for protected-file
// treatment once inspector-gate (issue #4) exists.
const ConfigFileName = ".inspector.json"

// ErrNoCheckCommand means the repo has no usable check command configured.
// inspector refuses to run rather than silently doing nothing, because a
// repo that runs unprotected must not look identical to one that runs
// protected.
var ErrNoCheckCommand = errors.New("no check command configured")

// DefaultTimeout bounds how long a check command may run when the
// project doesn't set timeoutSeconds. SPEC.md has Fabrica invoking
// inspector unattended, where a stuck check must produce a refusal
// rather than hang the whole handoff forever - this is a sane default
// for that, not a tuned one; a project whose honest runtime runs
// longer sets its own timeoutSeconds.
const DefaultTimeout = 15 * time.Minute

// Config is the project's inspector configuration.
type Config struct {
	// Check is the project's own check command, run with `sh -c` from the
	// repo root. It is the project's definition of green, not inspector's -
	// inspector never guesses at a test runner or build tool.
	Check string `json:"check"`
	// TimeoutSeconds bounds how long Check may run before inspector
	// kills it and refuses rather than hanging forever. Zero (unset)
	// uses DefaultTimeout.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// Timeout returns the configured timeout, or DefaultTimeout if unset.
func (c *Config) Timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return DefaultTimeout
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// LoadConfig reads ConfigFileName from repoRoot. It returns
// ErrNoCheckCommand if the file is absent or names an empty check command.
func LoadConfig(repoRoot string) (*Config, error) {
	path := filepath.Join(repoRoot, ConfigFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCheckCommand
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ConfigFileName, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", ConfigFileName, err)
	}
	if strings.TrimSpace(cfg.Check) == "" {
		return nil, ErrNoCheckCommand
	}
	return &cfg, nil
}
