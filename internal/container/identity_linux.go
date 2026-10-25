//go:build linux

package container

import (
	"fmt"
	"os"
)

// HostIdentity reports the uid and gid this process holds in the host's
// own initial user namespace, in docker run `--user` order.
//
// Inside a container it is the answer to the only question that decides
// whether a staged host path is reachable: not which uid the container
// sees itself as, but which host user the kernel will check that path's
// permissions against. Under a user-namespace-remapping or rootless
// Docker daemon those differ; on a daemon that puts the container in no
// namespace at all they are the same number and this returns the euid
// and egid unchanged.
//
// This is a measurement rather than a derivation, deliberately: it reads
// the mapping the kernel is actually applying instead of reconstructing
// it from the daemon's configuration, which would have to be read from
// files inspector often cannot see and would be wrong in exactly the
// direction that grants a real host account access it should not have.
func HostIdentity() (int, int, error) {
	uid, err := mappedSelfID("uid_map", os.Geteuid())
	if err != nil {
		return 0, 0, err
	}
	gid, err := mappedSelfID("gid_map", os.Getegid())
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func mappedSelfID(name string, id int) (int, error) {
	path := "/proc/self/" + name
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("reading %s to resolve host identity: %w", path, err)
	}
	mapped, err := MapID(string(data), id)
	if err != nil {
		return 0, fmt.Errorf("resolving host identity from %s: %w", path, err)
	}
	return mapped, nil
}
