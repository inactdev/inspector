// Package dockertest is test support, imported only by tests: it makes the
// image a container-gated test runs part of what that test's gate means by
// "a usable runtime".
//
// container.EnsureAvailable answers whether there is a reachable daemon, which
// is the right question and only half of the one these tests need. They also
// need the image they are about to run, and the images they run are anonymous
// reads of a public registry that rate-limits anonymous pulls per source
// address - so a refusal arrives on a machine with a perfectly good daemon,
// through nothing the commit under test did. Without this, that refusal
// surfaces inside a test as an ordinary error and fails the suite; with it, the
// test skips exactly as it does when there is no daemon at all, carrying the
// registry's own words.
//
// See notes/20261025-ci-check-red-on-a-refused-image-pull.md.
package dockertest

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// pullAttempts is how many times an absent image is asked for. A registry
// refusal is usually either a momentary blip - worth one more ask - or a rate
// limit measured in the hour, which no amount of retrying inside a test run
// will outlast. Three attempts cover the first without pretending about the
// second.
const pullAttempts = 3

// pullTimeout bounds one attempt. The largest of these images is a couple of
// hundred megabytes, so this is generous for a working registry and short
// enough that a hanging one does not consume the suite's own time budget.
const pullTimeout = 150 * time.Second

// pullBackoff is the wait before each retry.
var pullBackoff = []time.Duration{2 * time.Second, 5 * time.Second}

var (
	obtainedMu sync.Mutex
	// obtained records the outcome per image, so a suite of sixty tests
	// against the same image asks the daemon once.
	obtained = map[string]error{}
)

// RequireImage skips the test unless image is present locally or can be
// obtained now. Call it after the package's own daemon check: this one assumes
// a reachable daemon and reports on the image alone.
func RequireImage(t *testing.T, image string) {
	t.Helper()
	if err := EnsureImage(image); err != nil {
		t.Skipf("the %s test image is unavailable, so there is no usable runtime for this test, skipping: %v", image, err)
	}
}

// EnsureImage obtains image once per process and reports what happened. It is
// exported for the rare caller that wants to decide for itself what an
// unobtainable image means; RequireImage is what tests want.
func EnsureImage(image string) error {
	obtainedMu.Lock()
	defer obtainedMu.Unlock()
	if err, known := obtained[image]; known {
		return err
	}
	err := obtainImage(image)
	obtained[image] = err
	return err
}

func obtainImage(image string) error {
	if exec.Command("docker", "image", "inspect", image).Run() == nil {
		return nil
	}
	var last error
	for attempt := 0; attempt < pullAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(pullBackoff[min(attempt-1, len(pullBackoff)-1)])
		}
		ctx, cancel := context.WithTimeout(context.Background(), pullTimeout)
		output, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput()
		cancel()
		if err == nil {
			return nil
		}
		last = fmt.Errorf("docker pull %s: %w: %s", image, err, lastLine(string(output)))
	}
	return last
}

// lastLine is the registry's actual complaint: a refused pull prints its
// progress first and the reason last.
func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
