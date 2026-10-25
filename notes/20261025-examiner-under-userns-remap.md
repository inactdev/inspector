# The examiner under Docker userns-remap

Issue #32. `userns-remap` maps container root to a different, unprivileged
host user. The examiner stages three things in `MkdirTemp` directories with
owner-only modes - the inputs it reads, the verdict it reads back, and the
agent it executes from its own image - so under a remapping daemon the
container's host identity can reach none of them.

This note records which of the issue's three candidate fixes was taken, and
the two things that decided the shape once the candidate was chosen.

## The candidate taken: resolve the identity, grant exactly it

The issue listed three, none chosen:

1. **Run the container as the host user**, so nothing has to be bridged.
2. **Resolve the remapped identity and grant exactly it**, excluding
   everything else.
3. **Hand the inputs over by a route that is not the host filesystem.**

**(1) is not available under a remapping daemon at all**, which is worth
writing down because it reads like the obvious answer. The daemon maps the
container's `[0, 65536)` onto the host's `[R, R+65536)`, where `R` is the
subordinate-id base from `/etc/subuid` - typically 100000 or higher. The
inviting host user's own uid (typically 1000) is *below* that base, so there
is no container uid that maps to it. The host user is simply not
representable inside the namespace, and no `--user` value produces it.

The version of (1) that was already in the code - `--userns host` plus
`--user <host uid>` - opts the container out of the remapping entirely. That
is the wrong default for the one component in this project whose whole
purpose is a sealed runtime: it discards the isolation the host
administrator configured. It is removed here on that ground alone.

**(3) works and was rejected on cost.** Staging into a named volume (or
streaming a tar in and the verdict out) touches no host permissions at all,
and the daemon's own data root is not group- or world-readable, so the
secrecy bar is met. But populating and draining a volume means overriding
the image's entrypoint to a shell and `tar`, which gives up "the locally
built runtime forces the trusted agent entrypoint", and it adds volume
lifecycle that leaks on a crash.

**(2) is what is implemented.** It grants the least: no new identity gains
access to anything unless a measurement proves it must, and then exactly one
uid gains exactly the permissions it needs.

## The identity is measured, not inferred

The grant target is the host uid the container process will actually run as.
Getting it wrong is the failure mode the issue warns about - it grants a real
host user access to the request, the guidebook and the verdict - so it is
read by measurement rather than derived from the daemon's configuration.

Two inferential routes were rejected:

- Parsing `docker info`'s `DockerRootDir` basename (`/var/lib/docker/
  231072.231072`). Docker's own documentation uses that directory as the
  way to *verify* remapping is on, so the naming is real, but that the
  `info` response points at the namespaced path is a daemon implementation
  detail, not a documented contract.
- Reading `/etc/docker/daemon.json` for the remap user and resolving it
  through `/etc/subuid`. Both files are commonly unreadable to the user
  running inspector.

Instead the agent - the trusted entrypoint, from the already
fingerprint-verified image - is run once with `--host-identity` and the same
isolation and `--user` arguments as the real run. It resolves its own euid
and egid through `/proc/self/uid_map` and `/proc/self/gid_map` and prints the
host pair. `user_namespaces(7)` is explicit that a process reading its own
map file sees field two in its parent namespace, which for a Docker
user-namespace container is the host's initial namespace. That is the
answer, directly, with no Docker internals in the path.

The same measurement collapses the common case: on a daemon with no
remapping the container reports the host pair inspector already runs as, so
there is nothing to grant and nothing changes. The grant only ever happens
when the measurement says it is needed.

## Granting exactly one uid, without widening anything

`chown` to the measured uid needs `CAP_CHOWN`; inspector is not root. Adding
group or other bits is the mistake the issue names, and it fails silently.
What a non-privileged owner *can* do is add a POSIX ACL entry, so the staged
paths get one named-user entry each and keep their owner-only base modes:

| path             | granted |
| ---------------- | ------- |
| input directory  | `--x`   |
| each input file  | `r--`   |
| output directory | `--x`   |
| `verdict.json`   | `-w-`   |

`--x` on the directories rather than `r-x` is deliberate: the agent opens
fixed names, so it never needs to enumerate either directory. Combined with
`--x` on the output directory, the container holds write permission on
exactly the one file inspector created for it and cannot create, replace or
unlink anything beside it - and `-w-` rather than `rw-` because it only ever
writes the verdict, never reads one back.

Only the uid is granted anything, never the gid. A POSIX ACL's named-user
entry decides access on its own whenever the uid matches, so granting the
group as well would add a named group - access for a class of users rather
than the one identity.

A grant on the staged paths is not sufficient on its own: reaching a file
means traversing every directory above it, so a private `TMPDIR` leaves them
out of reach however they are granted. That is refused by name, pointing at
the directory, rather than fixed by granting traversal up a chain of
directories inspector does not own - which would open whatever else is in
them.

Written with `setxattr(2)` on `system.posix_acl_access`, encoded per
`include/uapi/linux/posix_acl_xattr.h`, using only the standard library -
this repository has no third-party dependencies and should not grow one for
this. The kernel validates the structure in `posix_acl_valid()` (entry
order, and a mask whenever a named entry is present) and returns `EINVAL`
for anything malformed, so a successful write is itself a correctness
signal; the entry is read back and compared regardless, because a filesystem
with extended attributes disabled is a real configuration and has to fail
loudly rather than leave the paths unreachable.

**The gotcha a reviewer will hit:** once a file carries an extended ACL, its
`st_mode` group bits report the ACL *mask*, not group access. A file granted
`r--` reads as mode `0440` in `ls -l` and `os.FileMode`. The real group
entry is still `---`, so no group member gains anything - but a test that
asserts `mode & 0o077 == 0` will fail, and has to decode the ACL instead.

## Why the verdict is now written in place

The agent used to publish the verdict by writing a temporary file in the
output directory and renaming it over `verdict.json`. Under remapping that
leaves the published file owned by the container's host identity with
owner-only mode, and **inspector cannot read it back** - it is not the
owner, so it can neither read the file nor add an ACL entry to it.

The reverse grant is not available either: the agent cannot add an ACL entry
for inspector's uid, because `posix_acl_valid()` requires every named id to
map into the caller's user namespace, and inspector's host uid does not.

So the agent now opens the `verdict.json` inspector created and writes
through it. The inode, its owner, and its mode stay inspector's. Nothing is
lost: `writeResult` runs at most once per examination and inspector only
reads the file after the container has exited, so the rename bought
atomicity against a reader that does not exist. What it gains is the tighter
output-directory grant above.

## Not yet verified live

Issue #32 asks for this to be verified on a real remapping daemon rather
than reasoned about, and that has not happened: the machine this was built
on has no container runtime, no way to install one, and no privilege to
configure a nested daemon. `TestExaminerStagedAccessUnderUsernsRemap`
is the verification, committed, and it skips loudly - naming
`{"userns-remap": "default"}` and `DOCKER_HOST` - wherever no such daemon is
reachable. Until one records a pass, `userns-remap` stays declared
unsupported in README and the refusal that names it stays in place.

What *is* verified without such a daemon: the ACL encoding against the real
kernel (which rejects a malformed one), the whole granted permission layout,
the mapping arithmetic against real `uid_map` shapes, the agent's
`--host-identity` answer, and that the ordinary non-remapped path grants
nothing at all.

## Still the same shape elsewhere

The project check container has this defect too, and is not fixed here. Its
workspace (`container.ResolveWorkspace`) and sanitized git config
(`container.ResolveGitMounts`) are `MkdirTemp` directories with owner-only
modes, and `container.New` does not pass `--userns host`, so under a
remapping daemon the check command cannot read `/workspace` either. It is a
harder problem rather than the same one: its read-only source mounts are the
*user's own repository files*, and inspector may not add ACL entries to a
judged project's tree. Tracked as its own work, not folded in here.
