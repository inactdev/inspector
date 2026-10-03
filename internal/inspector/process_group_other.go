//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package inspector

import (
	"fmt"
	"os/exec"
	"runtime"
)

func validatePublicationPlatform() error {
	return fmt.Errorf("green publication is unavailable on %s because inspector cannot guarantee termination of git's complete process tree", runtime.GOOS)
}

func configureProcessGroup(*exec.Cmd) {}

func killProcessGroup(*exec.Cmd) error {
	return validatePublicationPlatform()
}
