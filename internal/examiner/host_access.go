package examiner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/inactdev/inspector/internal/container"
)

// grantStagedAccess makes the sealed inputs and the staged verdict
// reachable by the container that is about to run, and by nothing else.
//
// On a Docker daemon that puts the container in a user namespace - the
// `userns-remap` configuration a hardened Linux host is likely to have
// on, and rootless Docker - the container's host identity is a different
// unprivileged account from the one inspector runs as. The inputs, the
// verdict, and the directories holding them are created with owner-only
// modes, so that account can reach none of them and every examination
// fails (issue #32).
//
// What it does NOT do is widen those modes until the container can read
// them. These paths carry the request, the feature map, the always-true
// list and the verdict, and the container they feed holds an API key; a
// staging directory readable by every other account on the host would
// break nothing and so would never be noticed. Exactly one uid is
// granted exactly the access it needs, and a host where even that cannot
// be arranged refuses rather than loosens.
//
// It is also a no-op in the ordinary case. The container reports which
// host identity it actually runs as, and on a daemon that remaps nothing
// that is the identity inspector already owns these paths as, so nothing
// is granted and nothing about the staged paths changes.
func grantStagedAccess(ctx context.Context, prepared PreparedInputs, outputDir, verdictPath, runtimeUser, runtimeImageID string) error {
	// Only a Linux host can need this, and only a Linux host can do
	// anything about it. A user namespace is a Linux daemon feature, and
	// inspector refuses a remote Docker endpoint already
	// (container.ErrRemoteDocker), so on Linux the uid the container
	// reports and the uid owning these paths are numbers from the same
	// machine's one user table. Elsewhere - Docker Desktop on macOS or
	// Windows - a bind mount crosses a file-sharing backend that decides
	// reachability itself rather than by matching uids, so a mismatch
	// would say nothing and acting on it would be a guess. Left exactly
	// as it was there, which is the behavior that works today, and the
	// identity is not even measured since nothing would use it.
	if runtime.GOOS != "linux" {
		return nil
	}
	uid, err := containerHostUID(ctx, runtimeUser, runtimeImageID)
	if err != nil {
		return err
	}
	return applyStagedGrant(prepared, outputDir, verdictPath, uid)
}

// applyStagedGrant is grantStagedAccess once the container's host uid is
// known, split out so the decision and the grant can be exercised
// without a Docker daemon.
func applyStagedGrant(prepared PreparedInputs, outputDir, verdictPath string, uid int) error {
	if uid == os.Getuid() {
		return nil
	}
	// Granting access to the staged paths is pointless if the container
	// cannot traverse the directories above them, which is what a
	// private TMPDIR produces. Named here rather than worked around by
	// granting traversal up a chain of directories inspector does not
	// own, which would open whatever else is in them.
	for _, staged := range []string{prepared.Dir, outputDir} {
		blocked, err := container.UnreachableAncestor(staged)
		if err != nil {
			return fmt.Errorf("checking whether the examiner container can reach %q: %w", staged, err)
		}
		if blocked != "" {
			return fmt.Errorf("%w: the examiner container runs as host user %d, which cannot traverse %q to reach the staged %q - point TMPDIR at a directory it can reach, such as /tmp", container.ErrAccessGrantUnsupported, uid, blocked, staged)
		}
	}

	grants := []struct {
		path string
		perm container.Perm
		what string
	}{
		// Traversal only, never Read: the agent opens fixed names, so it
		// has no reason to be able to enumerate either directory.
		{prepared.Dir, container.Execute, "the sealed input directory"},
		{outputDir, container.Execute, "the verdict output directory"},
		// Write without Read, and granted on the file rather than its
		// directory, so the container can fill in the verdict inspector
		// staged for it and cannot create, replace or unlink anything
		// beside it.
		{verdictPath, container.Write, "the staged verdict"},
	}
	for _, name := range inputFileNames() {
		grants = append(grants, struct {
			path string
			perm container.Perm
			what string
		}{filepath.Join(prepared.Dir, name), container.Read, "sealed input " + name})
	}
	for _, grant := range grants {
		if err := container.GrantUser(grant.path, uid, grant.perm); err != nil {
			return fmt.Errorf("the examiner container runs as host user %d, which cannot reach %s: %w", uid, grant.what, err)
		}
	}
	return nil
}

// containerHostUID asks the container which host user it runs as.
//
// Measured rather than derived: the alternative is reconstructing the
// daemon's user-namespace mapping from its own configuration, which
// means reading files inspector often cannot see, and naming the wrong
// host user there would hand a real account access to private staged
// material. The agent reads the mapping the kernel is applying to it and
// prints the answer.
//
// Run only after ensureRuntimeImage has confirmed the image is the
// reviewed one: this reads a value out of that image and acts on it, so
// it must not run against an image whose contents are still unverified.
func containerHostUID(ctx context.Context, runtimeUser, runtimeImageID string) (int, error) {
	cmd := newHostIdentityCommand(ctx, runtimeUser, runtimeImageID)
	defer cmd.Cleanup()
	// Answer and diagnostics kept apart, unlike the fingerprint check
	// next door which compares one combined stream: a daemon that prints
	// an unrelated warning on stderr must not turn a host this works on
	// into a refusal.
	var answer, diagnostics bytes.Buffer
	cmd.Stdout = &answer
	cmd.Stderr = &diagnostics
	err := cmd.Run()
	if killErr := cmd.KillError(); killErr != nil {
		return 0, fmt.Errorf("reading the examiner container's host identity: container could not be stopped: %v; it may still be running", killErr)
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("reading the examiner container's host identity: %w", ctx.Err())
		}
		return 0, fmt.Errorf("reading the examiner container's host identity: %v: %s", err, strings.TrimSpace(diagnostics.String()))
	}
	// The gid is read for the same reason the agent reports it - it is
	// the other half of the identity - but only the uid is granted
	// anything. A POSIX ACL's named-user entry decides access on its own
	// whenever the uid matches, so granting the gid as well would add a
	// named group, which is access for a class of users rather than the
	// one identity this is allowed to open anything to.
	uid, _, err := parseHostIdentity(answer.String())
	if err != nil {
		return 0, err
	}
	return uid, nil
}

// parseHostIdentity reads the agent's `--host-identity` answer. It is
// strict on purpose: the number it returns decides which host user is
// granted access to the staged inputs, so anything it cannot read
// exactly is an error rather than a default.
func parseHostIdentity(output string) (int, int, error) {
	trimmed := strings.TrimSpace(output)
	ids := strings.Split(trimmed, ":")
	if len(ids) != 2 {
		return 0, 0, fmt.Errorf("the examiner container reported its host identity as %q, which is not uid:gid", trimmed)
	}
	uid, err := strconv.Atoi(ids[0])
	if err != nil || uid < 0 {
		return 0, 0, fmt.Errorf("the examiner container reported host uid %q, which is not a uid", ids[0])
	}
	gid, err := strconv.Atoi(ids[1])
	if err != nil || gid < 0 {
		return 0, 0, fmt.Errorf("the examiner container reported host gid %q, which is not a gid", ids[1])
	}
	return uid, gid, nil
}
