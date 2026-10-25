//go:build linux

package container

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// aclEntry is one decoded POSIX ACL entry. Tests decode the ACL rather
// than reading the file mode on purpose: a path carrying an extended ACL
// reports the ACL's mask in its st_mode group bits, so the mode says
// "group readable" for a file no group member can read. The entries are
// the truth.
type aclEntry struct {
	tag  uint16
	perm Perm
	id   uint32
}

func decodeACL(t *testing.T, path string) []aclEntry {
	t.Helper()
	raw, err := readACL(path)
	if err != nil {
		t.Fatalf("reading the POSIX ACL of %q: %v", path, err)
	}
	if len(raw) < aclHeaderSize || (len(raw)-aclHeaderSize)%aclEntrySize != 0 {
		t.Fatalf("POSIX ACL of %q is %d bytes, which is not a header plus whole entries", path, len(raw))
	}
	if version := binary.LittleEndian.Uint32(raw[:aclHeaderSize]); version != aclXattrVersion {
		t.Fatalf("POSIX ACL of %q has version %d, want %d", path, version, aclXattrVersion)
	}
	var entries []aclEntry
	for offset := aclHeaderSize; offset < len(raw); offset += aclEntrySize {
		entries = append(entries, aclEntry{
			tag:  binary.LittleEndian.Uint16(raw[offset : offset+2]),
			perm: Perm(binary.LittleEndian.Uint16(raw[offset+2 : offset+4])),
			id:   binary.LittleEndian.Uint32(raw[offset+4 : offset+8]),
		})
	}
	return entries
}

func entryFor(t *testing.T, entries []aclEntry, tag uint16) aclEntry {
	t.Helper()
	for _, entry := range entries {
		if entry.tag == tag {
			return entry
		}
	}
	t.Fatalf("POSIX ACL %#v has no entry tagged %#x", entries, tag)
	return aclEntry{}
}

// requireACLSupport skips loudly when the file system under the test's
// temporary directory cannot carry POSIX ACLs, the same tolerance the
// Docker tests extend to a machine with no runtime. GrantUser refusing
// there is the designed behavior, not a failure to reproduce.
func requireACLSupport(t *testing.T) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatalf("writing the ACL support probe: %v", err)
	}
	if err := GrantUser(probe, 65533, Read); err != nil {
		t.Skipf("POSIX ACLs are unavailable on this test file system, skipping: %v", err)
	}
}

func TestGrantUser_GrantsOneUidAndWidensNothing(t *testing.T) {
	requireACLSupport(t)
	path := filepath.Join(t.TempDir(), "request.md")
	if err := os.WriteFile(path, []byte("the request"), 0o400); err != nil {
		t.Fatalf("staging the input file: %v", err)
	}

	const remapped = 231072
	if err := GrantUser(path, remapped, Read); err != nil {
		t.Fatalf("GrantUser(%q, %d, Read) = %v", path, remapped, err)
	}

	entries := decodeACL(t, path)
	if named := entryFor(t, entries, aclTagUser); named.id != remapped || named.perm != Read {
		t.Fatalf("named entry = %#v, want uid %d with read only", named, remapped)
	}
	if owner := entryFor(t, entries, aclTagUserObj); owner.perm != Read {
		t.Fatalf("owner entry = %#v, want the file's own read-only owner access", owner)
	}
	if group := entryFor(t, entries, aclTagGroupObj); group.perm != 0 {
		t.Fatalf("group entry = %#v, want no group access at all", group)
	}
	if other := entryFor(t, entries, aclTagOther); other.perm != 0 {
		t.Fatalf("other entry = %#v: the staged request must not become readable by other users on the host", other)
	}
	if mask := entryFor(t, entries, aclTagMask); mask.perm != Read {
		t.Fatalf("mask entry = %#v, want read so the named grant is effective", mask)
	}
	for _, entry := range entries {
		if entry.tag == aclTagUser {
			continue
		}
		if entry.tag == aclTagGroup {
			t.Fatalf("POSIX ACL %#v grants a named group, which would widen access beyond one uid", entries)
		}
	}
}

func TestGrantUser_DirectoryGetsTraversalWithoutListing(t *testing.T) {
	requireACLSupport(t)
	dir := filepath.Join(t.TempDir(), "input")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("staging the input directory: %v", err)
	}

	const remapped = 100000
	if err := GrantUser(dir, remapped, Execute); err != nil {
		t.Fatalf("GrantUser(%q, %d, Execute) = %v", dir, remapped, err)
	}

	entries := decodeACL(t, dir)
	if named := entryFor(t, entries, aclTagUser); named.id != remapped || named.perm != Execute {
		t.Fatalf("named entry = %#v, want uid %d with traversal only - the agent opens fixed names and must not be able to list the directory", named, remapped)
	}
	if other := entryFor(t, entries, aclTagOther); other.perm != 0 {
		t.Fatalf("other entry = %#v: the staged input directory must stay unreachable to other users on the host", other)
	}
}

func TestGrantUser_KeepsExistingGroupAccessEffective(t *testing.T) {
	requireACLSupport(t)
	path := filepath.Join(t.TempDir(), "shared")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatalf("staging the file: %v", err)
	}

	if err := GrantUser(path, 231072, Write); err != nil {
		t.Fatalf("GrantUser = %v", err)
	}

	entries := decodeACL(t, path)
	if group := entryFor(t, entries, aclTagGroupObj); group.perm != Read {
		t.Fatalf("group entry = %#v, want the read access the mode already described", group)
	}
	if mask := entryFor(t, entries, aclTagMask); mask.perm != Read|Write {
		t.Fatalf("mask entry = %#v, want read+write so neither the existing group access nor the new named grant is reduced", mask)
	}
}

func TestGrantUser_RefusesPathThatAlreadyCarriesAnACL(t *testing.T) {
	requireACLSupport(t)
	path := filepath.Join(t.TempDir(), "already")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("staging the file: %v", err)
	}
	if err := GrantUser(path, 100000, Read); err != nil {
		t.Fatalf("first GrantUser = %v", err)
	}

	err := GrantUser(path, 100001, Read)
	if !errors.Is(err, ErrAccessGrantUnsupported) {
		t.Fatalf("GrantUser on a path with an existing ACL = %v, want ErrAccessGrantUnsupported - replacing it would drop entries this cannot see", err)
	}
	if named := entryFor(t, decodeACL(t, path), aclTagUser); named.id != 100000 {
		t.Fatalf("named entry = %#v, want the refused call to have changed nothing", named)
	}
}

func TestGrantUser_RefusesNonsenseGrants(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "staged")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("staging the file: %v", err)
	}
	for name, grant := range map[string]struct {
		path string
		uid  int
		perm Perm
	}{
		"negative uid":  {path, -1, Read},
		"no permission": {path, 100000, 0},
		"missing path":  {filepath.Join(dir, "absent"), 100000, Read},
	} {
		if err := GrantUser(grant.path, grant.uid, grant.perm); !errors.Is(err, ErrAccessGrantUnsupported) {
			t.Fatalf("GrantUser with %s = %v, want ErrAccessGrantUnsupported", name, err)
		}
	}
}

func TestEncodeACL_KernelRejectsAMalformedEntryOrder(t *testing.T) {
	requireACLSupport(t)
	path := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("staging the file: %v", err)
	}

	// posix_acl_valid() enforces the entry order encodeACL emits, so a
	// wrong one is rejected rather than stored. This proves the kernel is
	// validating what GrantUser writes, which is why a successful
	// Setxattr is itself evidence the encoding is right.
	scrambled := make([]byte, aclHeaderSize)
	binary.LittleEndian.PutUint32(scrambled, aclXattrVersion)
	entry := make([]byte, aclEntrySize)
	binary.LittleEndian.PutUint16(entry[0:2], aclTagOther)
	binary.LittleEndian.PutUint32(entry[4:8], aclUndefinedID)
	scrambled = append(scrambled, entry...)

	if err := syscall.Setxattr(path, aclAccessAttr, scrambled, 0); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("setting a malformed POSIX ACL = %v, want EINVAL", err)
	}
}
