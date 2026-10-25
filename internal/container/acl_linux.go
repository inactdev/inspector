//go:build linux

package container

import (
	"encoding/binary"
	"errors"
	"os"
	"syscall"
)

// POSIX ACL extended-attribute ABI, from
// include/uapi/linux/posix_acl_xattr.h and include/uapi/linux/posix_acl.h.
// The on-attribute layout is one little-endian uint32 version followed by
// entries of {uint16 tag, uint16 perm, uint32 id}, regardless of the
// host's own byte order - the kernel headers type every field as __le*.
const (
	aclAccessAttr = "system.posix_acl_access"

	aclXattrVersion = 0x0002

	aclTagUserObj  = 0x01
	aclTagUser     = 0x02
	aclTagGroupObj = 0x04
	// aclTagGroup, a named group, is deliberately never emitted - the
	// whole point is to grant one uid, not a class of users - and is
	// named here so a test can assert its absence.
	aclTagGroup = 0x08
	aclTagMask  = 0x10
	aclTagOther = 0x20

	aclHeaderSize = 4
	aclEntrySize  = 8

	// aclUndefinedID is ACL_UNDEFINED_ID: the e_id of every entry whose
	// tag already says who it applies to.
	aclUndefinedID = ^uint32(0)
)

// GrantUser gives exactly the host user uid exactly perm on path, and
// changes nothing else about who can reach it: the path keeps its own
// owner-only mode, and no group or world access is added.
//
// This exists because a container under a user-namespace-remapping
// Docker daemon runs as a host user that is neither inspector nor
// representable inside the container, so a staged input or output
// directory created with an owner-only mode is unreachable to it (issue
// #32). The two obvious alternatives are both wrong: chown needs
// CAP_CHOWN, which inspector does not have, and loosening the mode until
// it works makes the staged request, guidebook and verdict readable by
// every other account on the host - a failure that breaks nothing and
// so would never be noticed.
//
// A named-user POSIX ACL entry is what a non-privileged owner can add,
// and it names one uid rather than a class of users. Note that a path
// carrying one reports the ACL's *mask* in its st_mode group bits, so it
// reads as group-accessible to ls(1) and os.FileMode while the ACL's
// group entry stays empty - decode the ACL to see real group access, do
// not read the mode.
//
// Errors wrap ErrAccessGrantUnsupported when nothing here can grant the
// access at all, so a caller can refuse with that reason rather than
// treating it as an ordinary failure.
func GrantUser(path string, uid int, perm Perm) error {
	if uid < 0 {
		return unsupportedGrant("%q cannot be granted to uid %d, which is not a host user", path, uid)
	}
	if perm == 0 {
		return unsupportedGrant("%q was asked to grant uid %d nothing at all", path, uid)
	}
	info, err := os.Stat(path)
	if err != nil {
		return unsupportedGrant("%q could not be read before granting uid %d %s access: %v", path, uid, perm, err)
	}

	// Rebuilding the whole access ACL from the mode is only faithful for
	// a path with no extended ACL yet, which every path inspector stages
	// is - it created them moments earlier. One that already carries
	// entries is refused rather than silently rewritten, because the
	// entries this cannot see are exactly the ones it would drop.
	if existing, err := readACL(path); err == nil && len(existing) > 0 {
		return unsupportedGrant("%q already carries a POSIX ACL, which granting uid %d %s access would replace", path, uid, perm)
	} else if err != nil && !errors.Is(err, syscall.ENODATA) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EOPNOTSUPP) {
		return unsupportedGrant("%q could not be checked for an existing POSIX ACL before granting uid %d %s access: %v", path, uid, perm, err)
	}

	mode := info.Mode().Perm()
	owner := Perm(mode >> 6 & 0o7)
	group := Perm(mode >> 3 & 0o7)
	other := Perm(mode & 0o7)
	// Every named entry's effective access is itself AND the mask, so the
	// mask has to carry the union of the named grant and whatever group
	// access the mode already described, or one of the two would be
	// silently reduced.
	encoded := encodeACL(owner, uid, perm, group, perm|group, other)
	if err := syscall.Setxattr(path, aclAccessAttr, encoded, 0); err != nil {
		return unsupportedGrant("%q could not be granted uid %d %s access through a POSIX ACL: %v", path, uid, perm, err)
	}
	// A file system that stores the attribute without honoring it would
	// leave the container unable to reach its own inputs with nothing to
	// point at, so the result is read back rather than assumed. Compared
	// by effect rather than byte-for-byte, so this verifies the two
	// things that matter - the one uid got exactly its grant, and nobody
	// else got anything - instead of an encoding the kernel is free to
	// store however it likes.
	written, err := AccessOf(path)
	if err != nil {
		return unsupportedGrant("%q accepted a POSIX ACL granting uid %d %s access but could not report it back: %v", path, uid, perm, err)
	}
	if written.Users[uid] != perm {
		return unsupportedGrant("%q reports uid %d holding %s after being granted %s", path, uid, written.Users[uid], perm)
	}
	if len(written.Users) != 1 || len(written.Groups) != 0 {
		return unsupportedGrant("%q reports %d individually granted uids and %d groups after granting uid %d alone", path, len(written.Users), len(written.Groups), uid)
	}
	if written.Group != group || written.Other != other {
		return unsupportedGrant("%q widened from group %s / other %s to group %s / other %s while granting uid %d %s access", path, group, other, written.Group, written.Other, uid, perm)
	}
	return nil
}

// AccessOf reports who can reach path besides its owner, reading the
// POSIX ACL when there is one and the mode when there is not. Named
// entries and the group entry are reduced by the ACL's mask, so the
// permissions it reports are effective rather than nominal.
func AccessOf(path string) (Access, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Access{}, err
	}
	mode := info.Mode().Perm()
	access := Access{Group: Perm(mode >> 3 & 0o7), Other: Perm(mode & 0o7), Users: map[int]Perm{}, Groups: map[int]Perm{}}

	raw, err := readACL(path)
	if err != nil {
		if errors.Is(err, syscall.ENODATA) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			return access, nil
		}
		return Access{}, err
	}
	if len(raw) == 0 {
		return access, nil
	}
	if len(raw) < aclHeaderSize || (len(raw)-aclHeaderSize)%aclEntrySize != 0 {
		return Access{}, unsupportedGrant("%q has a POSIX ACL of %d bytes, which is not a header plus whole entries", path, len(raw))
	}
	if version := binary.LittleEndian.Uint32(raw[:aclHeaderSize]); version != aclXattrVersion {
		return Access{}, unsupportedGrant("%q has a POSIX ACL of version %d, want %d", path, version, aclXattrVersion)
	}
	// The group entry replaces the mode's group bits once an ACL exists,
	// because those bits then carry the mask instead.
	access.Group = 0
	mask := Perm(Read | Write | Execute)
	named := map[int]Perm{}
	groups := map[int]Perm{}
	for offset := aclHeaderSize; offset < len(raw); offset += aclEntrySize {
		tag := binary.LittleEndian.Uint16(raw[offset : offset+2])
		perm := Perm(binary.LittleEndian.Uint16(raw[offset+2 : offset+4]))
		id := binary.LittleEndian.Uint32(raw[offset+4 : offset+8])
		switch tag {
		case aclTagGroupObj:
			access.Group = perm
		case aclTagOther:
			access.Other = perm
		case aclTagMask:
			mask = perm
		case aclTagUser:
			named[int(id)] = perm
		case aclTagGroup:
			groups[int(id)] = perm
		}
	}
	access.Group &= mask
	for uid, perm := range named {
		access.Users[uid] = perm & mask
	}
	for gid, perm := range groups {
		access.Groups[gid] = perm & mask
	}
	return access, nil
}

func readACL(path string) ([]byte, error) {
	size, err := syscall.Getxattr(path, aclAccessAttr, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	read, err := syscall.Getxattr(path, aclAccessAttr, buf)
	if err != nil {
		return nil, err
	}
	return buf[:read], nil
}

// encodeACL lays out the five entries one named grant needs, in the
// order posix_acl_valid() requires: ACL_USER_OBJ, named ACL_USER,
// ACL_GROUP_OBJ, ACL_MASK, ACL_OTHER. The kernel rejects any other
// order, and requires the mask entry precisely because a named entry is
// present.
func encodeACL(owner Perm, uid int, named, group, mask, other Perm) []byte {
	buf := make([]byte, aclHeaderSize, aclHeaderSize+5*aclEntrySize)
	binary.LittleEndian.PutUint32(buf, aclXattrVersion)
	appendEntry := func(tag uint16, perm Perm, id uint32) {
		entry := make([]byte, aclEntrySize)
		binary.LittleEndian.PutUint16(entry[0:2], tag)
		binary.LittleEndian.PutUint16(entry[2:4], uint16(perm))
		binary.LittleEndian.PutUint32(entry[4:8], id)
		buf = append(buf, entry...)
	}
	appendEntry(aclTagUserObj, owner, aclUndefinedID)
	appendEntry(aclTagUser, named, uint32(uid))
	appendEntry(aclTagGroupObj, group, aclUndefinedID)
	appendEntry(aclTagMask, mask, aclUndefinedID)
	appendEntry(aclTagOther, other, aclUndefinedID)
	return buf
}
