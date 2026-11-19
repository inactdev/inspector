# Build arguments read from the repository under check

Built 2026-11-19. Amends
`notes/20261025-project-image-from-committed-dockerfile.md` (the check image is
built from the project's own committed Dockerfile, tagged by that file plus its
bases),  `notes/20261103-the-repo-named-build-context.md` (that build also gets
the repository under check as the named `repo` context) and
`notes/20261104-named-build-secrets-for-the-check-image-build.md` (the one
input that is in no file). This adds the input that *is* in a file, and whose
value has to end up in the image's tag: a version the repository declares.

## The plan, and the alternative that was rejected

Chosen, safest first: `.inspector.json` gains `buildArgs`, a map from a build
argument name to **where in the repository under check its value lives** -
`{"file": ".ruby-version"}` for the whole first non-empty line, or
`{"file": "go.mod", "pattern": "(?m)^go (\\S+)$"}` for a value inside a bigger
file. Each value is read **out of the commit being checked**, with
`git cat-file blob <commit>:<path>`, never off the working tree; the resolved
`NAME=value` pairs are sorted, folded into the existing `ProjectImageTag` hash
beside the Dockerfile bytes and the bases' image IDs, substituted into every
`FROM` line before its base image is resolved, and passed to the build as one
`docker build --build-arg NAME=value` each. A missing file, a path that is not
a file in that commit, a pattern that matches nothing, or a value inspector
will not put on a command line refuses the run - naming the argument and the
file - before any image is built.

Rejected: reading each value off the working tree with `os.ReadFile`, inside
`internal/container` where `buildSecrets` lives. One function shorter, and the
working tree is clean by the time a check runs - but clean does not mean
"every file is the commit's". A gitignored `.ruby-version` passes the
clean-tree refusal and would have set the version the check image was built
with while the commit declared nothing of the sort, which is the same
tag-cannot-see-it failure this whole file exists to prevent. Reading git's
object instead also means a declared path cannot leave the commit: a symlink
in the tree reads as its own link text, not as the file it points at, so
`{"file": "creds"}` can never reach `/home/you/.aws/credentials`.

Also rejected: a short hash of the pairs, or the literal values, appended to
the tag as a second component. The tag is already one content-derived hash of
everything the image depends on; a second component would have to be
explained, sanitized for the tag charset, and kept in step with the first. The
pairs go into the same hash - and deliberately contribute nothing when a
project declares none, so every existing project's tag is unchanged and
nothing rebuilds for this.

## What changed

`.inspector.json`:

```json
{
  "check": "bundle exec rspec",
  "buildArgs": {
    "RUBY_VERSION": { "file": ".ruby-version" },
    "BUNDLER_VERSION": { "file": "Gemfile.lock", "pattern": "BUNDLED WITH\\s+(\\S+)" }
  }
}
```

`Dockerfile.inspector`:

```dockerfile
ARG RUBY_VERSION
FROM ruby:${RUBY_VERSION}
ARG BUNDLER_VERSION
RUN gem install bundler -v "${BUNDLER_VERSION}"
```

and the build inspector runs becomes

    docker build -f <Dockerfile.inspector> -t inspector-check:<hash> \
      --build-context repo=<repo root> \
      --build-arg BUNDLER_VERSION=2.5.6 --build-arg RUBY_VERSION=3.4.13 \
      <main context>

one `--build-arg` per declared name, in name order (which is also the order
the tag hashes them in, so neither depends on a Go map's iteration).

## Why the value comes out of the commit

The point of this key is that a changed version rebuilds the image. That only
holds if the value is a fact about the commit - and `internal/inspector.Run`
already refuses a dirty working tree, so for every *tracked* file the two are
the same bytes. Reading the git object rather than the file closes the two gaps
the clean-tree refusal leaves open: a gitignored file (ignored, so not dirty,
so not refused, and in no commit), and a symlink (whose blob is the link text,
so a declared path cannot read anything the commit does not contain). It also
means the value a report prints is the value the named commit declares, which
is what a reviewer reading the report after the fact needs it to be.

With `--terms`, the files are read from the repository under check, never from
the terms folder - the same rule the `repo` build context follows, and for the
same reason: the lockfile that decides the image is the project's own.

## Why the values are in the tag, and in the report

In the tag because the whole feature is pointless otherwise. The tag hashes
the Dockerfile and its bases; a version living in `.ruby-version` is in
neither, so without folding the pairs in, `FROM ruby:${RUBY_VERSION}` would
reuse yesterday's image under today's version - and reuse happens exactly when
the Dockerfile reads no build context, which is the case a version-only
Dockerfile is.

In the report, with their values, because a version is not a secret and
because "which environment judged this commit" is what that part of the report
is for. `buildSecrets` is names-only for the opposite reason, and the two must
not be confused: a credential belongs under `buildSecrets`, where docker reads
it from inspector's environment and nothing inspector writes ever holds it. A
`--build-arg` value is recorded in the built image's own metadata
(`docker history`), printed in the run's console line, and written into
`.inspector/`'s reports - so anything that must not appear in those places is
not a build argument. That is stated in README rather than guarded against:
inspector cannot tell a version from a token by looking at it.

## `FROM ruby:${RUBY_VERSION}` resolves to `ruby:3.4.13`

`parseDockerfile` refused any `FROM` naming its base through a variable,
because an unresolved reference cannot be resolved to the digest the tag is
derived from. It still refuses - but now only after substituting the resolved
build arguments into the line, so the ordinary spelling of this feature works
and `ProjectImage.Bases` (and the report's `imageBases`) records
`ruby:3.4.13 = sha256:...` rather than the unexpanded text.

The substitution is `${NAME}` and `$NAME` for a declared name, and nothing
else. `${NAME:-default}`, a name no `buildArgs` entry declares, and a value
that itself contains `$` all leave a `$` behind and are refused by the same
message as before, now naming `buildArgs` as the way to declare the value.
Refusing is the safe direction: the failure being avoided is inspector
resolving one base image while docker builds from another.

Two residuals, both loud rather than silent:

- A Dockerfile that uses `${RUBY_VERSION}` in `FROM` without declaring
  `ARG RUBY_VERSION` before it is a file **docker** expands to the empty
  string, while inspector expands it to the value. Inspector resolves and
  pulls `ruby:3.4.13`; docker then fails on `FROM ruby:` with its own message,
  captured in the build log and surfaced as a refusal. Teaching inspector
  docker's global-`ARG` scope rules would turn that into a better-worded
  refusal, not into a different outcome, so it is not done.
- A global `ARG RUBY_VERSION=3.3` default with no `buildArgs` entry is still
  refused rather than read as 3.3. Reading Dockerfile defaults would make the
  tag depend on a value nobody passed, and the project can always declare the
  argument or pin the base literally.

## The refusals, and what they name

Every one of them wraps `container.ErrBuildArgUnavailable`, which wraps
`ErrProjectImageUnavailable`, so a caller that only knows the general sentinel
still refuses and never reports a verdict - exit `2`, nothing built, nothing
judged, exactly like a declared build secret that is not set. Each names the
argument and the file, because that pair is what the person reading it has to
go look at:

- the file is not in the commit under check, or is not a file in it;
- the file holds no non-empty line (no pattern), or the pattern matches
  nothing;
- the pattern does not compile, or does not have exactly one capture group -
  there is no rule for which group would be the value, so one is required
  rather than assumed;
- the file is larger than `MaxBuildArgFileBytes` (4 MiB). Its size is asked
  for before its content, with the same `git cat-file` call that establishes
  the path is in the commit at all, because the whole object is read into
  memory to be matched against - a declared path aimed at a repository's
  large tracked asset refuses by name rather than making inspector the
  problem;
- the value is empty, is not valid UTF-8, carries a control character
  (a captured newline, a binary blob), or is longer than
  `MaxBuildArgValueBytes`;
- the name is not `[A-Za-z0-9_]+`, which is what fits in `NAME=value` without
  an escaping rule nobody can audit.

One residual of reading the object rather than the file: a repository whose
`.gitattributes` runs a clean/smudge filter or `text=auto` stores different
bytes than the working tree holds. For a version file the difference is line
endings, which the no-pattern read trims away; a project's own pattern against
a CRLF-stored file has to allow for `\r` itself, and if it does not, it
matches nothing and says so. Nothing here silently guesses.

Patterns are Go's `regexp` (RE2), matched against the whole file content so a
value on the line *after* its marker - `BUNDLED WITH\n   2.5.6` - is reachable
at all. RE2 has no backtracking, so a project's own pattern cannot hang the
run; `(?m)` and `(?s)` are available for line-anchored and
dot-matches-newline matching. The first match wins.

## The same shape elsewhere, left as it is

Two other project declarations are read off the working tree rather than out
of the commit, and both stay that way on purpose:

- `LoadConfig` reads `.inspector.json` with `os.ReadFile`. With `--terms` that
  file is outside every repository inspector was handed, so there is no commit
  to read it from - the one case issue #69 exists for.
- `ProjectDockerfile` reads `Dockerfile.inspector` off disk, but refuses one
  git does not track (`ErrUncommittedProjectImage`), which closes the
  gitignored-file gap a different way. Its content is hashed into the tag, so
  a disk-versus-commit difference would also have to survive the clean-tree
  refusal.

Nothing else reads a value out of the repository to decide how the check
image is built; the `repo` build context hands docker the working tree, which
`notes/20261103-the-repo-named-build-context.md` already records as clean
by the time it runs.

## Where this lives

- `internal/inspector/config.go` - the `buildArgs` key and `BuildArgSource`.
- `internal/inspector/buildarg.go` - `ResolveBuildArgs`: the git read, the
  pattern, the refusals. In the inspector package because reading a commit is
  that package's business (`repo.go` owns every git call), and because
  `internal/container` must stay a container package that never runs git.
- `internal/container/buildarg.go` - the resolved `BuildArg` pair, the name
  and value rules both halves apply (`ValidateBuildArgs`, which
  `BuildProjectImage` re-applies itself so "no unvalidated name reaches a
  docker flag" is the builder's own property), the `--build-arg` flags, and
  the `FROM` substitution.
- `internal/container/image.go` - `ProjectImageTag` folds the pairs in;
  `parseDockerfile` substitutes before resolving a base. The command-line
  assembler formerly called `buildArgs` is now `buildCommandArgs`, freeing
  that name for the thing docker calls a build argument.
- `internal/inspector/run.go` resolves them before `container.EnsureAvailable`,
  for the same reason the Dockerfile and the build secrets are resolved there:
  a project's own declaration that cannot be read is worth saying on a machine
  with no Docker at all.
- `Report.ImageBuildArgs` records them, beside `Image` and `ImageBases`.
