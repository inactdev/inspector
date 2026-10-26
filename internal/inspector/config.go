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

// ErrRetiredImageKey means .inspector.json still names an "image". The
// check container is built from the project's own committed Dockerfile
// (ProjectDockerfileName) and from nothing else, so a configured image
// name no longer selects anything. Refused rather than ignored: a config
// naming one image while the check runs in another is exactly the
// worker-box/judge-box divergence issue #47 closes.
var ErrRetiredImageKey = errors.New("the \"image\" field has been retired")

// ErrRetiredNetworkKey means .inspector.json still carries the "network"
// opt-in, with any value. The check container runs with no network and
// there is nothing left to opt into (issue #47): a project's dependencies
// belong in the image its own Dockerfile builds, which is what the opt-in
// was waiting for.
var ErrRetiredNetworkKey = errors.New("the \"network\" opt-in has been retired")

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
	// Image and Network are two retired fields, kept here only so a
	// config that still carries either is refused by name rather than
	// read past in silence. The image the check runs in now comes from
	// the project's own committed ProjectDockerfileName and from nothing
	// else, and the container has no network with no way to ask for one
	// (issue #47). json.RawMessage rather than their old types because
	// what is refused is the key's presence, whatever its value: a
	// "network": false that inspector accepted would read as a setting
	// that still does something.
	Image   json.RawMessage `json:"image,omitempty"`
	Network json.RawMessage `json:"network,omitempty"`
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
	// TestPaths lists the glob patterns naming which of the project's own
	// files are tests (issue #44). Unlike ProtectedPaths, BOTH halves read
	// this one: inspector judges every changed test file matching it and
	// posts the result as the TestsStatusContext commit status, and
	// inspector-gate reads the base branch's copy to know which protected
	// path that status is allowed to clear. Same glob dialect as
	// ProtectedPaths; absent means inspector judges nothing and posts no
	// such status, exactly as before this field existed.
	TestPaths []string `json:"testPaths,omitempty"`
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
// ErrNoCheckCommand if the file is absent or names an empty check
// command, and ErrRetiredImageKey or ErrRetiredNetworkKey if it still
// carries one of the two fields issue #47 retired.
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
	if cfg.Image != nil {
		return nil, ErrRetiredImageKey
	}
	if cfg.Network != nil {
		return nil, ErrRetiredNetworkKey
	}
	return &cfg, nil
}
