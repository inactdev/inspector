package dockertest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// shim is a docker shim put first on PATH - the same trick
// internal/inspector's own tests use for docker's kill path - so the retry and
// memoization below are tested without a daemon, a registry, or a real pull.
// It answers `image inspect` with present, fails `pull` until succeedAt, and
// records every call it was given.
const shim = `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKERTEST_CALLS"
if [ "$1" = "pull" ]; then
	attempts=$(cat "$DOCKERTEST_PULLS" 2>/dev/null || echo 0)
	attempts=$((attempts + 1))
	printf '%s' "$attempts" > "$DOCKERTEST_PULLS"
	if [ "$attempts" -lt "$DOCKERTEST_SUCCEED_AT" ]; then
		echo "trying to pull"
		echo "toomanyrequests: too many requests to the registry" >&2
		exit 1
	fi
	exit 0
fi
[ "$DOCKERTEST_PRESENT" = "yes" ]
`

func shimDocker(t *testing.T, present bool, succeedAt int) (calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	callsFile := filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(shim), 0o755); err != nil {
		t.Fatalf("writing the docker shim: %v", err)
	}
	// First on PATH, not the whole of it: the shim itself runs ordinary
	// commands, and so would anything these tests shell out to.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKERTEST_CALLS", callsFile)
	t.Setenv("DOCKERTEST_PULLS", filepath.Join(dir, "pulls"))
	t.Setenv("DOCKERTEST_SUCCEED_AT", strconv.Itoa(succeedAt))
	t.Setenv("DOCKERTEST_PRESENT", map[bool]string{true: "yes", false: "no"}[present])

	// Retries are real here but instant: the waits are what the suite would
	// otherwise sit through, not what is under test.
	original := pullBackoff
	pullBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { pullBackoff = original })

	return func() []string {
		content, err := os.ReadFile(callsFile)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(content)), "\n")
	}
}

func pullCount(calls []string) int {
	pulls := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "pull ") {
			pulls++
		}
	}
	return pulls
}

// TestEnsureImage_RetriesARefusedPull is why this exists: a registry that
// refuses once is not a verdict on the commit under test.
func TestEnsureImage_RetriesARefusedPull(t *testing.T) {
	calls := shimDocker(t, false, 3)

	if err := EnsureImage("retried-image"); err != nil {
		t.Fatalf("EnsureImage() error = %v, want the third attempt to have succeeded", err)
	}
	if got := pullCount(calls()); got != 3 {
		t.Errorf("docker pull ran %d times, want three attempts", got)
	}
}

// TestEnsureImage_ReportsTheRegistrysOwnReason - whoever reads the skip has to
// be able to tell a rate limit from a typo in an image name.
func TestEnsureImage_ReportsTheRegistrysOwnReason(t *testing.T) {
	shimDocker(t, false, 9)

	err := EnsureImage("refused-image")
	if err == nil {
		t.Fatal("EnsureImage() = nil for an image that was never obtained")
	}
	if !strings.Contains(err.Error(), "refused-image") || !strings.Contains(err.Error(), "toomanyrequests") {
		t.Errorf("EnsureImage() error = %v, want it to name the image and the registry's own words", err)
	}
}

// TestEnsureImage_AsksOnlyOncePerImage keeps a suite of sixty gated tests from
// becoming sixty pulls, and keeps a refusal from being retried per test.
func TestEnsureImage_AsksOnlyOncePerImage(t *testing.T) {
	calls := shimDocker(t, false, 9)

	first := EnsureImage("asked-once-image")
	before := len(calls())
	second := EnsureImage("asked-once-image")

	if first == nil || second == nil || first.Error() != second.Error() {
		t.Fatalf("EnsureImage() = %v then %v, want the same recorded outcome", first, second)
	}
	if got := len(calls()); got != before {
		t.Errorf("docker was asked %d more times on the second call, want none", got-before)
	}
}

// TestEnsureImage_PullsNothingWhenTheImageIsAlreadyThere - the common case is
// an image already on the machine, and it must cost one local question.
func TestEnsureImage_PullsNothingWhenTheImageIsAlreadyThere(t *testing.T) {
	calls := shimDocker(t, true, 9)

	if err := EnsureImage("present-image"); err != nil {
		t.Fatalf("EnsureImage() error = %v, want a present image accepted", err)
	}
	if got := pullCount(calls()); got != 0 {
		t.Errorf("docker pull ran %d times for an image already present", got)
	}
}

func TestLastLine(t *testing.T) {
	if got := lastLine("pulling\nmore pulling\ndenied: no\n"); got != "denied: no" {
		t.Errorf("lastLine() = %q, want the registry's last word", got)
	}
	if got := lastLine(""); got != "" {
		t.Errorf("lastLine(\"\") = %q, want empty", got)
	}
}
