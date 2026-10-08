//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package examiner

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPrepareInputsContext_RejectsFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "request")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("creating FIFO: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := PrepareInputsContext(ctx, Inputs{RequestPath: fifo})
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("PrepareInputsContext() = %v, want non-regular-file refusal", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PrepareInputsContext() blocked opening a FIFO")
	}
}
