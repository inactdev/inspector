# The commit under check is mounted read-only

Issue #27: the check command cannot modify tracked source at all, and gets a
throwaway writable workspace for the leftovers a real check legitimately
produces - installed packages, build output, coverage and test reports.

Enforced at the execution boundary, not verified afterwards, because #18's
five review rounds ended on the one defence a post-check snapshot can never
mount: a check can save a tracked file, modify it, pass against the modified
bytes, and restore it before exiting. Anything built on inspecting the tree
can be defeated by putting it back, so the capability is removed instead.

## The layout, and why it is this one

`/workspace` inside the container is no longer the repo. It is a fresh host
temporary directory - the run's writable workspace - carrying one bind mount
per top-level entry of the repo root:

- every top-level entry git tracks, plus `.git`, mounted **read-only**
- every other top-level entry (gitignored build output, installed
  dependencies, `.inspector/`) mounted read-write, exactly as before
- the workspace root itself writable, so a check that creates `dist/`,
  `node_modules/` or `coverage/` in its own tree keeps working with no
  project configuration at all

A bind mount cannot be unlinked or renamed from inside the container (`EBUSY`)
and unmounting needs `CAP_SYS_ADMIN`, which the check container does not have
and could not use as a non-root user anyway. So a read-only mounted path
cannot be replaced by a writable one of the same name either.

Docker applies all of a container's mounts in lexicographic order of their
destination, whatever order the flags arrive in, so a child destination always
lands on top of the already-mounted parent - the same behavior
`ResolveGitMounts` already relies on to lay a single file over a mounted git
directory. Two things follow worth keeping in mind: the flags are emitted
parent-first anyway so the result does not depend on that sort, and if a
nested mount ever failed to apply, the check command would find an empty file
or directory where its code should be and fail loudly, rather than find
writable source. One watch item: Docker Desktop 4.67.0 (Engine 29.3.1) on
macOS has a reported regression where overlapping bind mounts stop overlaying
under Compose; plain `docker run`, which is all inspector uses, is reported
unaffected.

## The alternative that was rejected

**A full overlay of the repo** - lowerdir the repo, upperdir a scratch
directory, every path under `/workspace` writable - is what "overlay-style,
zero project config" most literally describes, and it is what Docker's `local`
volume driver can be talked into with `volume-opt=type=overlay`.

It was rejected because overlayfs copies a lower file up on first write. A
check could therefore modify `src/x.go` in the upper layer, compile against
the modified bytes, pass, and have the change vanish with the discarded
scratch - the exact laundering this issue exists to close, and *less*
detectable than today, since the host tree never changes at all. Overlayfs has
no "deny copy-up of existing files, allow new files" mode, so there is no
variant of it that both keeps tracked source read-only and keeps the repo path
writable. Per-entry mounts are the only mechanism that gives both.

Also rejected: a scratch area at a separate path with projects pointing build
output at it (fails "ordinary projects must not need to change anything"), and
a symlink or hardlink farm of tracked files over a writable directory tree (a
check can `rm` a symlink and write its own file at that path, or write through
a hardlink into the real inode).

## Symlinks at the repo root, which this repository has

`CLAUDE.md -> AGENTS.md` is one, so this was not hypothetical. A symlink
cannot be mounted at all: Docker resolves the source path, so mounting one
mounts whatever it points at. Both available answers are imperfect and the
choice went to fidelity:

- **Mount the resolved target read-only** keeps the path immutable, but the
  container then sees a regular file where the commit records a link - `git
  status` in there reports a typechange, and a *linked directory* arrives as
  a second real copy of a tree the commit has once, which changes what a
  build walks. A judgment has to be about the commit's own tree.
- **Recreate the link** (what it does) is faithful - the link text git
  recorded, resolving through the read-only mount of its target - but the
  link itself sits in the writable workspace, so a check can remove it and
  write its own file at that one tracked path.

The residual is stated in README rather than hidden. Narrowing it needs
something other than a mount flag; nothing in Docker, and nothing short of
`CAP_LINUX_IMMUTABLE`, makes a single entry of a writable directory
unremovable. A link leading out of the repository, or to nothing, is refused
by name instead: honoring it would mean mounting a host path the commit does
not contain.

## What this covers and what it does not

Covered: no write to a tracked path can succeed - with the single symlink
exception below - and nothing the check writes reaches the repository. Reads
are unchanged, and so is the rest of containment: the mount still exposes the
repo and nothing else on the host.

Not covered, deliberately and stated in README:

- **Writes outside the repository entirely** are a different problem, named
  out of scope by the issue itself. The container remains the boundary there.
- **Files the check adds to its own tree** land in the discarded workspace
  rather than the repo. That is the point for build output, but it also means
  the publication recheck's "non-ignored addition" rule no longer sees an
  added file - the check cannot modify what it is judged on, but it can still
  add to it for the duration of its own run. Closing that needs a rule about
  what may appear in the workspace, not a mount flag, and is left open rather
  than half-built.

#18's dirty rule is kept exactly as it was, by ruling: it binds the result to
a commit, which is a different job from immutability.

## Sharp edges worth knowing

- **A symlink at the repo root is carried as a link, or refused by name** -
  see the section above. Anything at the repo root that is neither a directory
  nor a regular file is refused. Entries *inside* a top-level directory are
  unaffected either way: the mount is recursive.
- **New files inside a tracked directory fail too**, not just writes to
  tracked files: a read-only bind mount is read-only for the whole subtree,
  and no mount flag can distinguish "new file in `src/`" from "modified file
  in `src/`". Root-level build output, installed dependencies and written
  reports - the cases the issue names - are unaffected. A project that writes
  into a tracked directory (Python's `__pycache__` beside its sources is the
  common one) has to send that output elsewhere.
- **`.git` is read-only now for an ordinary clone too**, not only for the
  linked worktree whose shared git directory was already mounted read-only.
  `git status`, `log` and `diff` work; `git commit` fails on a read-only file
  system, which is the same answer a worktree already gave.
- **`/workspace` is now a file system boundary**, because the workspace and
  each source mount are separate file systems. git stops its upward search
  for a repository at such a boundary ("Stopping at filesystem boundary
  (GIT_DISCOVERY_ACROSS_FILESYSTEM not set)"), so a check running git from
  any subdirectory would have been told it is not in a repository at all -
  issue #31's defect by another route. `container.New` therefore exports
  `GIT_DISCOVERY_ACROSS_FILESYSTEM=1`; nothing else in that container is
  discoverable, so crossing the boundary can only find the repository that
  is meant to be found. Anything else that is `-xdev`-sensitive (`find
  -xdev`, `rsync -x`, `du -x`) sees the same boundary and stops at it.
- **The workspace is discarded once the run's report is written**, by ruling -
  so nothing a check installs or builds survives to the next run, and each
  check sees the commit plus its own writes. Existing *gitignored* paths are
  still mounted writable from the host, so a cached `node_modules` keeps
  working and keeps being reused.
