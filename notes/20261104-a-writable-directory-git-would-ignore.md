# A check may write where git would ignore a new file

Issue #84. Built 2026-11-04. Amends
`notes/20261023-source-read-only-during-checks.md`, whose sharp-edges section
told a project that writes into a tracked directory - Python's `__pycache__`
beside its sources was the example - to send that output elsewhere. For a
directory git would ignore a new file in, it no longer has to.

## The problem this closes

Issue #27 mounts each tracked top-level entry read-only **as a whole**, so one
tracked placeholder makes a directory unwritable in full. A Rails app tracks
`tmp/.keep`, `tmp/pids/.keep` and `log/.keep` and writes into all three at
boot: `tmp/local_secret.txt`, caches, pids, uploads, screenshots, log files.
Its suite therefore failed at `Errno::EROFS` on `/workspace/tmp/local_secret.txt`
before a single test ran - found by Fabrica's end-to-end test of a real Rails
fixture, and the real work repo tracks the same three files.

Issue #27's goal is that **no tracked file can be modified**. It never required
blocking writes git would ignore: nothing git ignores can be part of the commit
under check, so such a write cannot change what the check is judged on.

## The rule

For each tracked directory, starting at the repo root's tracked top-level
entries, inspector asks git whether it would ignore a new file placed directly
in it - `git check-ignore` against a probe path that does not exist
(`<dir>/inspector-write-probe`).

- **No** - mount the directory read-only as a whole, exactly as before.
- **Yes** - the directory becomes writable, each tracked file in it is mounted
  read-only **on its own**, and the same question is asked of each tracked
  subdirectory. Gitignored entries that already exist on the host keep their
  writable mount, as at the repo root.

Every tracked file stays read-only in every case; the answer only decides
whether a *new* file beside it succeeds or fails with `EROFS`.

## A writable directory is a mount, not a plain directory

The writable directory is a read-write bind mount of a throwaway host
directory (one per run, beside the workspace and discarded with it), not a
plain directory created inside the workspace.

That costs one extra mount per such directory and it is what makes the rule
honest. A bind-mounted *file* cannot be unlinked or renamed over from inside
the container - the fact the git config shadow mount already rides on - but a
plain directory holding those file mounts can itself be renamed, and the mounts
travel with it. A check could then `mv tmp tmp.old && mkdir tmp` and write its
own `tmp/.keep`, passing against bytes the commit does not contain: the
laundering issue #27 exists to close, arriving by a different door. Mounting the
directory makes that rename fail with `EBUSY`, which is exactly how
`/workspace` and every read-only source mount already resist it.

The stand-ins live in a second per-run temporary tree
(`inspector-writable-*`) and are numbered `1`, `2`, `3` rather than mirroring
the repository's paths. A mirrored layout would put a nested writable
directory's stand-in at exactly the path its own mount point has to occupy
inside its parent's stand-in - a directory mounted over itself, which is a
stranger thing to ask Docker for than two plainly separate paths.

### Alternatives rejected

- **A plain writable directory in the workspace** - one fewer mount per
  directory and less code, rejected for the rename above.
- **Copying the tracked files into a writable tree** - a copy can be edited,
  which gives up the only guarantee this layout has.
- **Keeping the whole-directory read-only mount and telling the project to
  write elsewhere** - what the note said before, rejected by issue #27's own
  bar that an ordinary project must pass with nothing configured. A Rails
  app's `tmp/` path is not the project's choice to move.
- **Deciding per write rather than per directory** - the layout has to be
  fixed before the check starts; no mount flag can distinguish a new file from
  a modified one at write time, which is why issue #27 reached for the
  directory granularity in the first place.

## How the mount count grows, and the cap

Before this, one mount per top-level entry of the repo root. Now, additionally,
per tracked directory git would ignore a new file in: **one** mount for the
directory itself, plus **one per entry directly inside it** - each tracked file
read-only, each existing gitignored entry writable, each tracked subdirectory
either one read-only mount (git would not ignore a new file there) or the same
expansion again.

So the growth is bounded by the contents of directories whose new files git
ignores, which is what a project puts in its ignore rules precisely because it
holds no source: the Rails case costs about ten mounts. A repository that
tracks thousands of files inside such a directory is the pathological case, and
it is capped rather than handed to Docker: `container.MaxWorkspaceMounts`
(1024) refuses the run by name, with the path that crossed the line. The cap is
a fixed constant with no config key, like `MaxOutputBytes` - a project that can
raise it can bring back the failure mode it exists to prevent, which here is a
`docker run` command line long enough to fail on `E2BIG` or a container that
takes minutes to start. A repo root with more than 1024 top-level entries would
be refused where it previously ran; nothing in that shape has been seen.

## Where it lives

`inspector.TopLevelSourceEntries` (`internal/inspector/repo.go`) asks git both
questions - what is tracked, and which tracked directories ignore a new file -
and returns the pruned tree of `container.SourceEntry`;
`container.ResolveWorkspace` (`internal/container/workspace.go`) turns that
into mounts and never asks git anything itself. The probe is one
`git check-ignore -z --stdin` call per level of writable directories, and an
exit status of 1 from it means "none of these" rather than a failure.

## Residuals

- **One probe name answers for every name.** A directory whose ignore rules
  cover only *some* new names - `*.log` rather than `/log/*` - answers "no" and
  stays read-only as a whole, so a project that writes `log/test.log` there
  still has to arrange its own output path. The probe asks one question per
  directory by design; asking per written file is not something a mount can do.
  The probe name is deliberately plain (`inspector-write-probe`, no leading
  dot) so a `.*` rule cannot make a source directory writable by accident.
- **The answer is the repository's own.** The probe runs with system and global
  git configuration disabled, exactly as the tracked-file listing beside it
  does, so a `core.excludesFile` in someone's home directory cannot change the
  layout a run gets. A directory ignored only by that file stays read-only.
- **A tracked file inside a writable directory can still be shadowed by a
  symbolic link**, the same residual issue #27 already states for the repo
  root: a tracked symlink is recreated as a link rather than mounted, so it
  lives in writable space and the check can replace that one name. Its target
  stays read-only.
- **An existing gitignored entry in there can be rewritten but not
  unlinked.** It is a bind mount from the host, so `rm -rf tmp/cache` fails on
  the directory itself while `rm -rf tmp/cache/*` works - the same property
  `node_modules` at the repo root has had since issue #27, now reachable one
  level deeper. A project that cleans by removing the directory has to empty
  it instead.
- **The repo root's rules for odd entries now reach one level deeper.** A
  directory laid out entry by entry is laid out the way the root is, so a
  symbolic link directly inside one is carried as a link, and one leading out
  of the repository - or an entry that is neither a directory nor a regular
  file - refuses the run by name instead of riding along inside a read-only
  mount as it did before. Loud rather than silent, and consistent with the
  root, but it is a behavior change for a repository that keeps such a link in
  an ignored directory.
- **New files still land in the discarded workspace**, so the publication
  recheck's "non-ignored addition" rule does not see them - unchanged by this,
  and the reason it is safe here is the stronger one: git ignores them, so they
  could never be part of the commit either way.
