# The check container is built from the project's own committed Dockerfile

Issue #47, the fourth and last part of #43. Decided and built 2026-10-25.

## What changed

The check container's image is no longer named in `.inspector.json`. Every
project commits **`Dockerfile.inspector`** at its repository root; inspector
builds that file at check time and runs the check in the result. A project
without one is refused by name, and so is a config that still carries either
retired key (`image`, `network`). The check container now always runs with
`--network none` - there is no opt-in left to honor.

One image, two boxes: Fabrica's worker box is this same image plus a thin
layer carrying the coding agent, so the environment the work is written in
cannot differ from the environment it is judged in. That is the whole reason
the network opt-in could finally go: dependencies are in the image, so nothing
inside the check container needs to install anything.

## The convention, and why a fixed path

`Dockerfile.inspector`, repo root, no config key to point somewhere else.
Owner ruling. A configurable path would be a second way to say the same thing,
and inspector-gate would then have to protect whatever that key named - a
moving target, decided by the branch under test, in a script that deliberately
reads only the base branch's own `.inspector.json`. A fixed name is protectable
by a constant.

The file has to be **tracked**, not merely present. `Run` already refuses a
dirty working tree, and that check uses `--untracked-files=all`, so an
untracked Dockerfile cannot survive it - except a gitignored one, which is
invisible to `git status`. So presence is checked on disk and committedness is
checked against the index, and the two refusals say which one failed.

## The tag: Dockerfile hash + resolved base image digests

Owner ruling: the built image is tagged by the Dockerfile's hash plus the base
image's digest, and the cached tag is reused until either changes.

    inspector-check:<sha256 of (Dockerfile bytes, then each FROM ref and the
                     image ID docker resolved it to)>

The base digest is read with `docker image inspect --format {{.Id}}` - the
same idiom `internal/examiner` already uses for its runtime image. `{{.Id}}`
is the local image config digest, which is always present; `.RepoDigests` is
empty for an image that was never pulled or pushed by digest, so it cannot be
the key.

A base that is not on the machine yet is pulled once (the build step may use
the network; the check container may not). A base that *is* on the machine is
**not** re-pulled. That is deliberate: it matches what `docker build` itself
does without `--pull`, so inspector's cache key always names the bytes the
build will actually use, and two runs of the same commit on the same machine
cannot disagree because a floating tag moved between them. Freshness is the
project's lever, declared in the committed Dockerfile - pin by digest and
there is nothing left to drift.

A `FROM` whose reference contains `$` is refused by name rather than guessed
at: inspector would have to resolve the variable to know which base it is
tagging, and an unresolved one would silently stop the tag from changing when
the base does. Same reasoning for a `# escape=` parser directive, which would
change how the file's line continuations parse. Fail closed, name the line.

## The one place the ruling's key cannot see a change, and what we do about it

The tag covers the Dockerfile and its bases. It does not cover the **build
context**. A Dockerfile that bakes dependencies in does exactly that:

    COPY package.json package-lock.json ./
    RUN npm ci

Reuse the tag for that file and a changed lockfile silently keeps yesterday's
dependencies. So: the tag is reused without building only when the Dockerfile
reads nothing from the build context. When it does - any `COPY`/`ADD` that is
not `--from=<stage>`, or any `RUN --mount` - `docker build` is run every time
and its own layer cache is what makes an unchanged rebuild cheap. The tag is
still the ruled one; what is conditional is only whether the build is skipped
outright. Deviation recorded in the pull request that introduced this.

## Residuals, in the same spirit as #27's

- The build context is the repo root, so a **gitignored** file at a path the
  Dockerfile copies can reach the image even though it is not part of the
  commit. The working tree is clean by the time this runs, so tracked content
  is the commit's - but `node_modules` or `dist` left from an earlier run are
  not. `.dockerignore` is the project's lever, and the issue #27 ruling
  already accepts gitignored leftovers as not-part-of-the-commit.
- Built images accumulate as `inspector-check:<hash>` tags. That is the cache;
  nothing prunes it.
- The build context being the repo root also means `.git` is in it, which
  `docker build` does not exclude on its own. A project with a large history
  that actually builds every run (one that copies from the context) wants
  `.git` in its `.dockerignore`. There is no flag to exclude it from outside
  the project, and writing a `.dockerignore` into a judged project is not
  something Inspector may do.
- A failed build's log is written to `.inspector/image-build.log` (notes only,
  never authority, like every other file in there) and named in the refusal.

## One consequence that had to come with it

`Dockerfile.inspector` joins `.github/**` and `.inspector.json` in
inspector-gate's `ALWAYS_PROTECTED` floor. The check environment used to be
named by `.inspector.json`'s `image` field, which that floor already covered;
leaving its replacement unprotected would have quietly narrowed what the gate
protects, and swapping the box a check runs in is another way to make a
failing check stop failing - SPEC.md section 8's exact subject.

## Also here, and deliberately not touched

Inspector's own `.inspector.json` still names `"image": "cimg/go:1.22"`, and
`script/check`'s preflight still tells a reader to "point .inspector.json's
image at one that does". Both are for the Client to apply: the first is the
retired key this change refuses, the second is a protected path whose edit the
gate cannot clear. `Dockerfile.inspector` at this repo's root is the committed
replacement and already carries the same base.

## Rejected

- **Keep honoring `image` as an override, ignore it otherwise.** Rejected: a
  config naming `cimg/go:1.22` while the check actually runs in whatever
  `Dockerfile.inspector` builds is exactly the worker-box/judge-box divergence
  this issue closes, and silently ignoring a declared field is the kind of
  quiet that this project refuses elsewhere (the recorded vendor default, the
  git config allowlist). The retired key is refused by name instead.
- **Expect a pre-built image to be present (or loaded from a tarball) and
  refuse when it is absent**, the way the examiner treats its own runtime
  image. Rejected for the project check: the examiner's image is inspector's
  own trusted artifact, built by inspector's own maintainers from pinned
  digests, so a build at examination time would be inspector building
  something it then trusts. A project's check image is the project's own
  declaration, it changes with the project's own commits, and requiring a
  human to remember `docker build` before every check would make the common
  case fail with "image not found" rather than just work. Building at check
  time also binds the image to the commit being judged, which is the whole
  point.
- **Always `docker build --pull`.** Rejected: it makes every run depend on a
  reachable registry and lets a floating base tag change the verdict between
  two runs of the same commit.
