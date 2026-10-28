# Bounding one run's output, and the retention policy for `.inspector/runs/`

Issue [#12](https://github.com/inactdev/inspector/issues/12), with the four
open numbers ruled by the owner on 2026-10-28. This note records what was
decided, what the numbers are, and the alternatives that were rejected, since
none of that is re-derivable from the constants themselves.

## The ruled numbers

They are **fixed constants, deliberately not configurable** - no `.inspector.json`
key, no flag. They live in one place each and are documented in README's report
section:

- `inspector.MaxOutputBytes` = 256 KiB per run, split `OutputHeadBytes` = 64 KiB
  head and `OutputTailBytes` = 192 KiB tail.
- `inspector.MaxRunsKept` = 20 run reports in `.inspector/runs/`; older ones are
  deleted on each new run.

Together they give the directory a stated ceiling of roughly 5 MB rather than an
unbounded one, which is the whole point of the issue.

## Why the cap lives in the capture writer, not at report-write time

The issue's first complaint is **memory**: a chatty suite printing tens of
megabytes cost several multiples of that for one run. Truncating when the report
is written would have left that exactly as it was - the full output would still
have been accumulated in a `bytes.Buffer` first, and the cap would only have
made the file smaller. So the bound is enforced where the bytes arrive:
`syncWriter` in `internal/inspector/check.go` now holds a fixed head slice and a
fixed-size ring for the tail, and never grows past the two. It is the same
writer the live stream tees through, so **what a human watching the run sees is
still complete and unbounded** - only the copy inspector keeps is bounded, and
the marker line says so.

Head *and* tail, because the reason a check failed is almost always the last
thing it printed, while the first thing it printed is what says which command
and toolchain ran. The marker between them names how many bytes were dropped
and what the whole run printed, so the report can never imply it holds
everything.

### Rejected: spool the full output to a file and truncate on read

Memory would have been bounded just as well, and nothing would have been lost.
Rejected because the owner's ruling is that the cap applies **everywhere,
including the on-disk artifact** - the point is bounded disk as much as bounded
memory, and a spool file only moves the growth from one to the other. It also
adds a temporary file to create, flush and discard on every error path, right
beside a run that already owns a throwaway workspace.

### Rejected: tail only, with no head

Simpler (one ring, no head slice, no marker arithmetic). Rejected because the
head is where the check command identifies itself, and a 64 KiB head costs
nothing against the 192 KiB that matters.

## Why the retention policy is a count

Count is what the owner ruled, and with a per-run byte cap now in place it is
also the dimension that needs no second guess: 20 runs can no longer mean 20
unbounded files. Age was rejected as the primary dimension because a repo
checked ten times in one afternoon and then left alone for a month would keep
nothing; a total-size ceiling was rejected because it is the same bound as
`MaxRunsKept * MaxOutputBytes` arrived at less predictably - a reader of
`.inspector/runs/` can count 20 files, but cannot see a size budget.

The issue's hard constraint is that pruning must never remove the run a commit
status currently refers to. Two things hold it:

1. Pruning runs **after** this run's report is written, keeping the newest
   `MaxRunsKept` - so the run being published is always among the kept, and the
   current status always has its notes.
2. More fundamentally, SPEC.md section 12: reports are notes, never authority.
   A green `inspector` status lives on the commit at GitHub and is what
   `inspector-gate` reads; removing an old report cannot invalidate or
   un-publish anything.

Pruning only ever removes files inspector itself wrote - a name of the shape
`<unix-seconds>-<short-sha>[-n].json`. Anything else a person put in there is
left alone, as is `latest.json`, which is not in `runs/`.

A prune that fails is reported as a loud warning on a run that otherwise
succeeded, never as a lost report: the newest record is worth more than the
bound, so `WriteReport` returns the written path alongside an error wrapping
`ErrRunsNotPruned`, and `run.go` turns that one error into `Result.Warning`
rather than into "the report could not be saved."

## The double write stays, because it has readers

The issue also asked whether one of the two per-run serializations could be
dropped. It cannot: `.inspector/latest.json` is the file Fabrica copies onto the
task record (see `notes/20261022-record-the-test-reviewers-reasons.md`), it is
what a green status's own description points a reader at
(`cmd/inspector/main.go`), and README documents it. Both writes have readers, so
both stay - bounded now at 256 KiB each instead of unbounded.

## The same shape elsewhere, left standing

`internal/alwaystrue/run.go` captures each falsifier container's combined output
into a plain `bytes.Buffer` and only then cuts it to `maxEvidenceBytes` (2000) -
so its memory is unbounded for the same reason this issue existed, and its 2000
bytes are head-only, which is the "keeps the boring part" flaw the issue names.
It is left as it is deliberately: the shape of a line's `evidence` is owned by
issue #24's ruling, and changing which 2000 bytes it keeps changes that
artifact. It is recorded here so the next agent finds it on purpose rather than
by accident.

`internal/container.BuildProjectImage` captures `docker build`'s whole output
into a `strings.Builder` and `inspector.WriteImageBuildLog` writes all of it to
`.inspector/image-build.log`, so a project whose Dockerfile installs noisily can
still cost one run as much memory and disk as it prints. Bounded by its own
nature in a way the check command is not - that file is overwritten per run
rather than accumulated, and a build's output is not a test suite's - and it
belongs to issue #47's artifact rather than this one, so it is left alone and
recorded here instead.

Every other `bytes.Buffer` capture in the tree reads a `git` or `docker`
subcommand's own short answer, not arbitrary project output, and is not this
shape.
