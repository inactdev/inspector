package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnreachableAncestor_NamesAnOwnerOnlyDirectoryAbovePath(t *testing.T) {
	// The private-TMPDIR shape: a staging directory whose own grant
	// cannot help because something above it denies traversal.
	root := t.TempDir()
	staged := filepath.Join(root, "staged")
	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatalf("creating the staged directory: %v", err)
	}

	blocked, err := UnreachableAncestor(staged)
	if err != nil {
		t.Fatalf("UnreachableAncestor(%q) = %v", staged, err)
	}
	if blocked == "" {
		t.Fatalf("UnreachableAncestor(%q) found nothing blocking, but it sits under owner-only %q", staged, root)
	}
	if !strings.HasPrefix(staged, blocked+string(filepath.Separator)) {
		t.Fatalf("UnreachableAncestor(%q) = %q, which is not an ancestor of it", staged, blocked)
	}
	info, err := os.Stat(blocked)
	if err != nil {
		t.Fatalf("reading the reported directory: %v", err)
	}
	if info.Mode().Perm()&0o001 != 0 {
		t.Fatalf("UnreachableAncestor(%q) = %q, which is mode %04o and traversable after all", staged, blocked, info.Mode().Perm())
	}
}

func TestUnreachableAncestor_FindsNothingWhenTheWholeChainIsTraversable(t *testing.T) {
	root := t.TempDir()
	staged := filepath.Join(root, "staged")
	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatalf("creating the staged directory: %v", err)
	}
	// Open every owner-only directory between the staged one and the
	// first already-traversable ancestor, which is what staging under
	// /tmp looks like on an ordinary host.
	for dir := root; ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("reading %q: %v", dir, err)
		}
		if info.Mode().Perm()&0o001 == 0 {
			if err := os.Chmod(dir, info.Mode().Perm()|0o001); err != nil {
				t.Skipf("cannot make %q traversable to set this case up, skipping: %v", dir, err)
			}
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}

	blocked, err := UnreachableAncestor(staged)
	if err != nil {
		t.Fatalf("UnreachableAncestor(%q) = %v", staged, err)
	}
	if blocked != "" {
		t.Fatalf("UnreachableAncestor(%q) = %q, want nothing once every ancestor is traversable", staged, blocked)
	}
}

func TestUnreachableAncestor_ReportsAMissingDirectory(t *testing.T) {
	if _, err := UnreachableAncestor(filepath.Join(t.TempDir(), "absent", "staged")); err == nil {
		t.Fatal("UnreachableAncestor returned no error for a path whose parent does not exist")
	}
}
