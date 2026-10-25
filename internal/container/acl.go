package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrAccessGrantUnsupported means a path inspector stages for a container
// cannot be granted to the container's host identity at all - no POSIX
// ACL support on this platform, or extended attributes disabled on the
// file system holding it. Callers must refuse the run rather than widen
// the mode until it works: these paths carry the request, the guidebook
// and the verdict, and a silently world-readable staging directory is
// the one failure here that nothing would ever report (issue #32).
var ErrAccessGrantUnsupported = errors.New("granting a container's host identity access to a staged path is unsupported here")

// Perm is the access one GrantUser call gives, in the same read/write/
// execute terms as a file mode. On a directory, Execute is permission to
// traverse it and Read is permission to list it - inspector grants
// traversal alone wherever the container opens fixed names, so a staged
// directory's contents cannot even be enumerated.
type Perm uint16

// The three POSIX ACL permission bits, from
// include/uapi/linux/posix_acl.h. Their values are the ABI, not an
// internal choice: they are written into the ACL entries GrantUser
// encodes.
const (
	Execute Perm = 0x01
	Write   Perm = 0x02
	Read    Perm = 0x04
)

func (p Perm) String() string {
	var parts []string
	if p&Read != 0 {
		parts = append(parts, "read")
	}
	if p&Write != 0 {
		parts = append(parts, "write")
	}
	if p&Execute != 0 {
		parts = append(parts, "execute")
	}
	if len(parts) == 0 {
		return "no access"
	}
	return strings.Join(parts, "+")
}

// Access is who can reach a path besides its owner. It exists because
// the mode alone cannot answer that question once a path carries a
// POSIX ACL: st_mode's group bits then report the ACL's mask rather
// than group access, so a file no group member can read still looks
// group-readable to ls(1) and os.FileMode. Read this instead of the
// mode when it matters whether something inspector staged is private.
type Access struct {
	// Group is the effective access members of the path's own group get.
	Group Perm
	// Other is the effective access every other account on the host gets.
	// For anything inspector stages for a container this must stay zero.
	Other Perm
	// Users is the effective access of each individually granted uid -
	// what GrantUser adds, and nothing else.
	Users map[int]Perm
	// Groups is the effective access of each individually granted group.
	// GrantUser never adds one, because a group is a class of users
	// rather than one identity; a path inspector stages that has any is
	// a path something else wrote an ACL on.
	Groups map[int]Perm
}

// UnreachableAncestor names the nearest directory above path that a host
// user unrelated to its owner cannot traverse, or "" when the whole
// chain is traversable.
//
// A grant on the path itself is not enough on its own: reaching a file
// means traversing every directory above it, so a staging directory
// created under a private TMPDIR stays out of reach of the container's
// host identity no matter what is granted on it. Rather than grant
// traversal up a chain of directories inspector does not own - which
// would open whatever else lives in them - the caller refuses and names
// the directory, which is something the person running it can act on.
//
// Conservative by construction: it asks only whether the directory is
// traversable by everyone, so an ancestor reachable through an ACL
// someone else already set is reported anyway. That errs toward
// refusing a run that might have worked, which is the safe direction
// here - the alternative is silently staging private material where the
// container cannot read it.
func UnreachableAncestor(path string) (string, error) {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil {
			return "", fmt.Errorf("checking whether %q can be reached: %w", dir, err)
		}
		if info.Mode().Perm()&0o001 == 0 {
			return dir, nil
		}
		if parent := filepath.Dir(dir); parent == dir {
			return "", nil
		}
	}
}

func unsupportedGrant(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAccessGrantUnsupported, fmt.Sprintf(format, args...))
}
