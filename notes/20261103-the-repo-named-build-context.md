# A project image can copy from the repository under check

Issue #81. Built 2026-11-03. Amends
`notes/20261025-project-image-from-committed-dockerfile.md` (the image the
check runs in) and the "The build context moves with the Dockerfile" section of
`notes/20261030-terms-outside-the-repo-and-a-run-with-no-stamp.md`, which said
a terms `Dockerfile.inspector` can reach nothing in the repository under check.
It can now, by one fixed name.

## What changed

Every project image build runs

    docker build -f <Dockerfile.inspector> -t inspector-check:<hash> \
      --build-context repo=<repo root> <main context>

so a Dockerfile can say

    COPY --from=repo Gemfile Gemfile.lock .ruby-version /deps/

The main context is unchanged and still follows the Dockerfile: the repository
root in the ordinary case, the terms folder with `-terms`. The named context is
the repository under check in **both** cases, which is the point - Fabrica's
`register` writes one Dockerfile and it has to work either way.

## Why this was needed at all

Found on the first real work-mode project, a Rails app whose terms
`fabrica register --terms` wrote. The check container has no network and no
opt-in left (#47), so a project's dependencies have to be in the image; baking
them in means reading the project's own lockfiles; and with `-terms` the build
context was the terms folder, which holds none of them. The only workaround was
copying `Gemfile.lock` into the terms folder by hand, where it goes stale the
moment the real one changes - a check that passes against dependencies the
repository no longer declares.

## The name is fixed, with no config key

Same ruling shape as `/workspace` and `/terms`. A config key would be a second
way to say the same thing, and `register` has to be able to write
`COPY --from=repo` without knowing anything about the project's config.

## A `repo` stage of the project's own is refused by name

Docker resolves `--from=key` against the named contexts **first**, ahead of a
build stage of the same name - documented behavior, not an accident: a named
context "overrides a stage called `name`". So a Dockerfile declaring
`FROM golang:1.22 AS repo` would have that stage silently replaced by the
repository under check, and whatever it was meant to contribute would quietly
vanish from the image the check then runs in. `parseDockerfile` refuses it,
quoting the offending line, in the same spirit as the `$`-in-`FROM` and
`# escape=` refusals already there. Compared case-insensitively, because docker
folds stage names.

The reverse direction is allowed rather than refused: `FROM repo` names the
context, contributes no base digest (there is no image to resolve), and counts
as reading a context. Before this change that line was refused as an
unresolvable base image, which was a confusing way to say something docker
itself supports.

## `--from=repo` counts as reading the build context

This is the part that would have been a silent wrong answer. The content-derived
tag hashes the Dockerfile and its bases, never a context, so an image is reused
without building only when the Dockerfile reads nothing from a context - that
rule is why `notes/20261025-project-image-from-committed-dockerfile.md` exists.
It treated **any** `--from=` as reading nothing, because `--from=<stage|image>`
genuinely is the file's own content. `--from=repo` is not: it is repository
content, exactly as invisible to the tag as the main context is. Left alone, a
changed lockfile would have kept yesterday's dependencies under the same tag -
the precise failure that rule was written to prevent, reintroduced through the
new door.

So `COPY --from=repo`, `ADD --from=repo` and `FROM repo` now mark the Dockerfile
as context-reading. `RUN --mount=from=repo` already did, because any
`RUN --mount` does.

Every `--from=` on a line is read, not only the first one. A line carrying two
of them is not a file docker builds, but which one a given builder would take if
it accepted the line is not something inspector should have to know: if any of
them names the repository, the Dockerfile counts as context-reading. The
asymmetry is the whole reason - over-matching costs one rebuild off docker's
layer cache, under-matching costs a check run against dependencies the
repository no longer declares.

## `.dockerignore`, plainly

Docker reads a local context's ignore file from **that context's own root**.
The repository's root is the root of the `repo` context, so the repository's own
`.dockerignore` is in the only place Docker would look for it, and nothing has
to be passed to apply it. What inspector cannot do is point a context at an
ignore file kept anywhere else: there is no flag for it (docker/buildx#3169),
and writing a `.dockerignore` into a judged project is not something Inspector
may do. Verified from Docker's documentation and buildx's own issue tracker
rather than against every BuildKit version in the wild - an older BuildKit that
applies no ignore file to a named context would transfer more of the repository
than the project asked for, which costs build time and can carry a gitignored
leftover into the image. That residual is the same one #47 already recorded for
the main context, now reachable in work mode too.

In particular `.git` is in the `repo` context, and `docker build` does not
exclude it. A repository with a large history whose Dockerfile actually reads
the context wants `.git` in its `.dockerignore` - true before this change for
an in-repo Dockerfile, true now for a terms one as well.

## BuildKit

A second build context needs BuildKit, which is what `docker build` has used by
default since Docker 23; the legacy builder cannot carry one. A machine whose
builder has no BuildKit fails the image build with docker's own message, which
inspector captures in the build log and surfaces in the refusal - a project
whose check image does not build has not been judged. That is a loud failure,
not a silent one: a legacy builder either rejects the flag outright or ignores
it and then fails on `--from=repo` with a pull error, and in neither case does
it produce an image quietly missing the files the Dockerfile copied.

`DOCKER_BUILDKIT=1` is deliberately **not** forced into the build's
environment. It would override a host setting someone chose, and on a machine
whose selected buildx builder does not load into the local image store it would
trade one loud failure for a stranger one (`docker run` against a tag that was
never loaded).

## Residuals

- In the ordinary in-repo case the repository is handed to docker twice, once
  as the main context and once as `repo`. Harmless, and newer BuildKit only
  transfers a context something actually reads.
- The named context is the working tree, like the main one: clean by the time
  this runs, so tracked content is the commit's, with the same gitignored-file
  residual #47 accepted.

## Rejected

- **Pass `--build-context` only when the Dockerfile appears to reference
  `repo`.** It would keep a non-BuildKit builder working for Dockerfiles that
  never use the feature. Rejected: the decision would then depend on inspector
  guessing right about every spelling that reaches the context, and a missed one
  fails the build with a confusing pull error instead of just working. Passing a
  context nothing reads costs nothing.
- **Copy the repository's files into the terms folder, or synthesise a combined
  build context.** That is the hand-copy this issue exists to kill, with
  inspector doing the copying - and it would have inspector writing into, or
  fabricating, the build context of the thing it judges.
- **Mount the repository into the build instead.** `docker build` has no mounts
  of its own; `RUN --mount` binds a context, so there is nothing here to choose
  between - a context is the mechanism.
