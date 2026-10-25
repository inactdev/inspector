package examiner

import (
	"fmt"

	runtimeidentity "github.com/inactdev/inspector"
	"github.com/inactdev/inspector/internal/container"
)

func RuntimeSourceFingerprint() string {
	return runtimeidentity.Fingerprint()
}

// ContainerHostIdentity reports the host uid:gid the agent is running
// as, for the host side to read back off stdout. Under a
// user-namespace-remapping Docker daemon that is not the uid the
// container sees itself as, and it is the only identity that can be
// granted access to the inputs and verdict inspector staged for it -
// see container.HostIdentity and issue #32.
func ContainerHostIdentity() (string, error) {
	uid, gid, err := container.HostIdentity()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", uid, gid), nil
}
