package container

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrIDUnmapped means an id inside a user namespace has no counterpart
// in the namespace the map file was read against, so there is no host
// user to name. Nothing may be guessed from that: the whole point of
// reading the mapping is to learn which host identity to grant access
// to, and the cost of naming the wrong one is granting a real host
// account access to private staged material (issue #32).
var ErrIDUnmapped = errors.New("id has no mapping outside its user namespace")

// MapID resolves id through the contents of a /proc/<pid>/uid_map or
// /proc/<pid>/gid_map file and returns the matching id outside that
// namespace.
//
// user_namespaces(7) documents each line as three whitespace-separated
// numbers: the start of a range inside the namespace, the start of the
// range it maps onto, and the length. A process reading its own map file
// sees the second field in its parent namespace, which for a container
// under a user-namespace-remapping or rootless Docker daemon is the
// host's own initial namespace. A process in the initial namespace reads
// the kernel's dummy "0 0 4294967295", which maps every id to itself.
func MapID(mapping string, id int) (int, error) {
	if id < 0 {
		return 0, fmt.Errorf("resolving id %d through a user namespace mapping: not an id", id)
	}
	for n, line := range strings.Split(mapping, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			return 0, fmt.Errorf("resolving id %d through a user namespace mapping: line %d has %d fields, want 3", id, n+1, len(fields))
		}
		inside, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("resolving id %d through a user namespace mapping: line %d namespace id %q: %w", id, n+1, fields[0], err)
		}
		outside, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("resolving id %d through a user namespace mapping: line %d outer id %q: %w", id, n+1, fields[1], err)
		}
		length, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("resolving id %d through a user namespace mapping: line %d range length %q: %w", id, n+1, fields[2], err)
		}
		if length == 0 {
			continue
		}
		if uint64(id) < inside || uint64(id)-inside >= length {
			continue
		}
		return int(outside + (uint64(id) - inside)), nil
	}
	return 0, fmt.Errorf("%w: %d", ErrIDUnmapped, id)
}
