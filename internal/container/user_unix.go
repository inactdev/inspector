//go:build unix

package container

import (
	"fmt"
	"os"
)

// HostUser returns the invoking process's own uid:gid, docker run
// `--user` syntax. See ResolveUser for why this is what New runs the
// check container as.
func HostUser() string {
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}
