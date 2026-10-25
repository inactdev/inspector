//go:build !linux

package container

import "errors"

// HostIdentity refuses off Linux. The mapping it reports is a Linux user
// namespace's, read from /proc; there is no equivalent to consult
// elsewhere, and a guessed answer would name the wrong host user. The
// examiner agent that calls this always runs inside a Linux container -
// this exists so the package still builds for a macOS or Windows host.
func HostIdentity() (int, int, error) {
	return 0, 0, errors.New("a process's host user namespace identity can only be read on Linux")
}
