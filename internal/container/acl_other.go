//go:build !linux

package container

import "os"

// GrantUser refuses on every platform but Linux. The host identity a
// container runs as can only differ from inspector's own when the daemon
// puts it in a user namespace, which is a Linux daemon feature, and
// POSIX ACLs are the Linux mechanism for granting one named uid access
// without widening the mode. Nothing here guesses an equivalent: a
// caller that needs the grant refuses instead (issue #32).
func GrantUser(path string, uid int, perm Perm) error {
	return unsupportedGrant("%q cannot be granted uid %d %s access: POSIX ACLs are only available on Linux", path, uid, perm)
}

// AccessOf reports who can reach path besides its owner. With no POSIX
// ACLs to read, the mode is the whole answer here, and no uid can have
// been granted access individually.
func AccessOf(path string) (Access, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Access{}, err
	}
	mode := info.Mode().Perm()
	return Access{Group: Perm(mode >> 3 & 0o7), Other: Perm(mode & 0o7), Users: map[int]Perm{}, Groups: map[int]Perm{}}, nil
}
