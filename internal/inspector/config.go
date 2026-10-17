package inspector

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

// ConfigFileName is the project-level config inspector reads to learn the
// project's own definition of green. It is committed to the repo, not
// gitignored, so it is visible for review - and inspector-gate protects
// it unconditionally (it is in that script's ALWAYS_PROTECTED floor), so
// a pull request cannot edit its own protectedPaths list away.
const ConfigFileName = ".inspector.json"

// ErrNoCheckCommand means the repo has no usable check command configured.
// inspector refuses to run rather than silently doing nothing, because a
// repo that runs unprotected must not look identical to one that runs
// protected.
var ErrNoCheckCommand = errors.New("no check command configured")

// ErrNoImage means the repo has no container image configured. The
// check command runs inside a container built from it - inspector never
// guesses one, the same way it never guesses the check command itself:
// the project's own toolchain has to be in the image or nothing runs.
var ErrNoImage = errors.New("no container image configured")

// DefaultTimeout bounds how long a check command may run when the
// project doesn't set timeoutSeconds. SPEC.md has Fabrica invoking
// inspector unattended, where a stuck check must produce a refusal
// rather than hang the whole handoff forever - this is a sane default
// for that, not a tuned one; a project whose honest runtime runs
// longer sets its own timeoutSeconds.
const DefaultTimeout = 15 * time.Minute

// Config is the project's inspector configuration.
type Config struct {
	// Check is the project's own check command, run with `sh -c` inside
	// the container. It is the project's definition of green, not
	// inspector's - inspector never guesses at a test runner or build
	// tool.
	Check string `json:"check"`
	// Image is the container image Check runs in - the project's own
	// toolchain, declared the same way Check itself is. inspector builds
	// nothing and ships nothing; see README.md.
	Image string `json:"image"`
	// Network, when true, gives the container network access. Default
	// false: the check command can reach the bind-mounted repo and
	// nothing else. This is a deliberately temporary opt-in - Fabrica's
	// own check currently needs it for `npm ci` when node_modules is
	// missing - that goes away once projects commit their own image
	// with dependencies baked in (issue #47); every run that uses it
	// logs as much. See README.md.
	Network bool `json:"network,omitempty"`
	// Memory, CPUs, and PIDsLimit bound the resources the check
	// container's cgroup may use - docker run's own --memory, --cpus,
	// and --pids-limit values. Empty/zero (the default) leaves
	// container.New to apply its own Default* constants, which match
	// Fabrica's worker box; see issue #43.
	Memory    string `json:"memory,omitempty"`
	CPUs      string `json:"cpus,omitempty"`
	PIDsLimit int    `json:"pidsLimit,omitempty"`
	// TimeoutSeconds bounds how long Check may run before inspector
	// kills it and refuses rather than hanging forever. Zero (unset)
	// uses DefaultTimeout.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// ProtectedPaths lists project-specific paths inspector-gate treats
	// as protected, in addition to its own fixed floor (.inspector.json
	// itself and .github/**). inspector itself never reads this field -
	// it exists only for inspector-gate, which reads it from the BASE
	// branch's copy of this file via the GitHub API, never the pull
	// request's own copy. One glob pattern per entry; what those
	// patterns match, and why Check is deliberately not protected
	// automatically, are owned by README.md's "inspector-gate" section.
	ProtectedPaths []string `json:"protectedPaths,omitempty"`
}

// Timeout returns the configured timeout, or DefaultTimeout if unset.
func (c *Config) Timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return DefaultTimeout
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// Limits returns the configured resource bounds for container.New,
// which applies its own defaults to whichever fields are left unset.
func (c *Config) Limits() container.Limits {
	return container.Limits{Memory: c.Memory, CPUs: c.CPUs, PIDsLimit: c.PIDsLimit}
}

// LoadConfig reads ConfigFileName from repoRoot. It returns
// ErrNoCheckCommand if the file is absent or names an empty check command,
// and ErrNoImage if it names an empty (or absent) image.
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
	if strings.TrimSpace(cfg.Image) == "" {
		return nil, ErrNoImage
	}
	return &cfg, nil
}
