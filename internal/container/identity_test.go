package container

import (
	"errors"
	"testing"
)

func TestMapID_ResolvesIdsThroughAUserNamespaceMapping(t *testing.T) {
	for name, probe := range map[string]struct {
		mapping string
		id      int
		want    int
	}{
		// What a process in the host's own initial namespace reads: the
		// kernel's dummy mapping, under which every id is itself. This is
		// the ordinary case, and it is why nothing is granted on a daemon
		// that remaps nothing.
		"initial namespace maps every id to itself": {"         0          0 4294967295\n", 1000, 1000},
		// What a container under `userns-remap` reads, with the daemon's
		// default dockremap range from /etc/subuid.
		"remapped container root":      {"         0     231072      65536\n", 0, 231072},
		"remapped non-root offset":     {"         0     231072      65536\n", 1000, 232072},
		"remapped last id in range":    {"         0     231072      65536\n", 65535, 296607},
		"second range wins its own id": {"0 100000 100\n500 200000 100\n", 505, 200005},
		// Rootless Docker's own mapping shape: the invoking user's uid
		// alone, then its subordinate range.
		"rootless root is the host user": {"0 501 1\n1 100000 65536\n", 0, 501},
		"rootless subordinate id":        {"0 501 1\n1 100000 65536\n", 7, 100006},
		"zero length range is skipped":   {"0 100000 0\n0 231072 65536\n", 0, 231072},
	} {
		got, err := MapID(probe.mapping, probe.id)
		if err != nil {
			t.Fatalf("%s: MapID(%q, %d) = %v", name, probe.mapping, probe.id, err)
		}
		if got != probe.want {
			t.Fatalf("%s: MapID(%q, %d) = %d, want %d", name, probe.mapping, probe.id, got, probe.want)
		}
	}
}

func TestMapID_RefusesAnIdItCannotResolve(t *testing.T) {
	// An id outside every mapped range has no host user behind it. This
	// is the case that must never produce a number: granting access to a
	// guessed uid hands a real host account the staged request and
	// verdict (issue #32).
	for name, probe := range map[string]struct {
		mapping string
		id      int
	}{
		"below the mapped range": {"1000 231072 10\n", 999},
		"above the mapped range": {"1000 231072 10\n", 1010},
		"nothing mapped at all":  {"\n", 0},
	} {
		if _, err := MapID(probe.mapping, probe.id); !errors.Is(err, ErrIDUnmapped) {
			t.Fatalf("%s: MapID(%q, %d) = %v, want ErrIDUnmapped", name, probe.mapping, probe.id, err)
		}
	}
	if _, err := MapID("0 1000 1\n", -1); err == nil {
		t.Fatal("MapID with a negative id returned no error")
	}
}

func TestMapID_RefusesAMalformedMapping(t *testing.T) {
	for name, mapping := range map[string]string{
		"two fields":        "0 231072\n",
		"four fields":       "0 231072 65536 7\n",
		"unparseable inner": "x 231072 65536\n",
		"unparseable outer": "0 x 65536\n",
		"unparseable size":  "0 231072 x\n",
		"negative size":     "0 231072 -1\n",
		"id past uint32":    "4294967296 0 1\n",
	} {
		if _, err := MapID(mapping, 0); err == nil || errors.Is(err, ErrIDUnmapped) {
			t.Fatalf("%s: MapID(%q, 0) = %v, want a parse error", name, mapping, err)
		}
	}
}

func TestHostIdentity_ReportsThisProcessOwnIdentity(t *testing.T) {
	uid, gid, err := HostIdentity()
	if err != nil {
		t.Skipf("this process's user namespace mapping is unreadable, skipping: %v", err)
	}
	if uid < 0 || gid < 0 {
		t.Fatalf("HostIdentity() = %d:%d, want real host ids", uid, gid)
	}
}
