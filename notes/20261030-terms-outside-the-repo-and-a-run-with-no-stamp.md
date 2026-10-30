# Terms from a folder outside the repo, and a run that posts no stamp

Issue #69, which makes issue #23's open question concrete. Built 2026-10-30.

## What it is for

Fabrica's work mode judges a company repository that may not carry anything
of inspector's: no `.inspector.json`, no `Dockerfile.inspector`, no check
script. So the three files move out of the repository into a folder beside it,
named per run with `-terms <dir>`, and a green run can be told to publish the
branch and the pull request while leaving no commit status behind at all
(`-no-stamp`).

Without either flag nothing changes. That is the bar both of them were built
to: the in-repo path is the same code reading the same two files from the
repository root, and the stamp is still what a green run posts unless a caller
says otherwise.

## What `-terms` moves, and what it deliberately does not

Moved: `LoadConfig`, `ProjectDockerfile`, and the `docker build` context all
resolve against the terms folder instead of the repository root. The folder is
mounted **read-only at `/terms`** (`container.TermsDir`, owner ruling - a
fixed path, like `/workspace`, because the project's check command names paths
under it), and the check command still runs from `/workspace`, where the
commit under check is mounted exactly as before.

Not moved:

- **The clean-tree refusal still applies to the repository.** A result is only
  honest when it is bound to a commit that matches what is on disk, and the
  terms folder has nothing to do with that. The terms folder itself is *not*
  required to be clean, or to be in a repository at all - it is configuration
  handed to this run, not the thing being judged.
- **The requirement that the terms be tracked is dropped, because there is no
  repository for them to be tracked in.** In the in-repo case `.inspector.json`
  is tracked by force of the clean-tree check (an untracked one makes the tree
  dirty) and `Dockerfile.inspector` is checked against the index outright
  (`ErrUncommittedProjectImage`). Neither question has an answer for a folder
  outside every repository inspector was handed, so for `-terms` the Dockerfile
  only has to be there. Said plainly because it is a real loss: with `-terms`,
  nobody reviewing the pull request sees the terms the work was judged under.
  That is the trade the issue asks for - the company repository may not carry
  them - and it is why inspector-gate is untouched: the gate protects in-repo
  terms by name from the base branch, and a repository that has none simply has
  nothing there for it to protect.
- **The report still goes in the judged repository**, under its gitignored
  `.inspector/`, as it always has. "Nothing may be added to the company
  repository" is about what gets committed; a self-ignoring notes directory is
  not that, and relocating it was not asked for.

One loose end worth naming rather than discovering: `protectedPaths` and
`testPaths` in a terms `.inspector.json` are read by the local half only.
`protectedPaths` was never read by inspector at all, and `testPaths` still
drives the changed-test judgment and its status - but `inspector-gate` reads
both from the *repository's* base-branch `.inspector.json`, which with `-terms`
does not exist, so it protects only its own hardcoded floor. That is the same
"the gate does not apply to a repository with no in-repo terms" fact from a
different angle, not a second mechanism.

## Why the terms folder may not be inside the repository, or hold it

`ResolveTerms` refuses either containment, by name. Both would undo something
that is not the terms folder's to undo:

- Terms **inside** the repository would be in-repo terms at a path
  inspector-gate does not protect - an unprotected `.inspector.json` under
  another name, which is exactly the "edit the thing that decides whether work
  passes" move SPEC.md section 8 exists to stop.
- Terms that **contain** the repository (pointing `-terms` at a parent
  directory, or at `$HOME`) would mount that whole ancestor into the check
  container read-only. The container's containment bar is that what a check can
  reach is declared, not inherited from wherever the repo happens to sit.

The terms mount is also deliberately **not** declared to git as a
`safe.directory`, unlike `ResolveGitMounts`' mounts: it is configuration, not
a git directory the check has to be able to read as a repository.

## The build context moves with the Dockerfile

`docker build`'s context is the directory the Dockerfile lives in - the
repository root in the ordinary case (unchanged), the terms folder with
`-terms`. A terms `Dockerfile.inspector` can therefore `COPY` its own
siblings and can reach nothing in the repository under check, which is the
right way round: the image is the environment, and baking the judged
repository's own content into it would make the environment depend on the
thing being judged.

One consequence worth knowing: the content-derived image tag is unchanged, so
two terms folders whose Dockerfiles and resolved bases are identical share one
image, exactly as two projects with identical Dockerfiles already do.

## `-no-stamp` suppresses every status, not only the green one

The flag's words in the issue are "posts no commit status". Taken literally:
neither `inspector` nor `inspector-tests` is posted. Suppressing the first
while posting the second would leave a commit carrying a judgment about its
tests and no judgment about its check, which says something untrue about the
run.

What still happens, per the issue: the staging push, the branch update, the
pull request, and the local report. The changed-test judgment itself still
runs and is still recorded and printed - it is a judgment, not a stamp, and
dropping it would silently lose a weakened-exam finding. The token is still
required, because the default branch is still read (to refuse it as a
publication target) and the pull request still needs it.

The report records `noStamp: true`, and a green run says so on stdout. A run
whose stamp was skipped on purpose and a run whose stamp was never confirmed
must not read the same way to anyone afterwards.
