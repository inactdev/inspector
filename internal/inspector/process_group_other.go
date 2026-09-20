//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package inspector

import "os/exec"

func configureProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
