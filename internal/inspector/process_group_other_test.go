//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package inspector

import (
	"strings"
	"testing"
)

func TestPushRefRefusesUnsupportedPlatformBeforeStartingGit(t *testing.T) {
	err := PushRefToRemote(t.TempDir(), "origin", "abc", "refs/heads/test")
	if err == nil {
		t.Fatal("expected publication to be refused")
	}
	if !strings.Contains(err.Error(), "complete process tree") {
		t.Fatalf("error = %q, want process-tree safety refusal", err.Error())
	}
	if strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("error = %q, git started before publication was refused", err.Error())
	}
}
