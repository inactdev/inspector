# The reviewer's test-change judgment (issue #44)

## Why this exists

`.inspector.json` protects `*_test.go`, so in this repository every pull request
that touches a Go test is red at `gate` until the owner reviews it and uses his
bypass (README's Examiner section, `protectedPathsNote`, fabrica#96). That blunt
red is correct policy and is not what #44 changes: it says *nothing* about
whether the test change was honest, so the owner pays full attention to every
one of them. SPEC.md section 5 is explicit that attention, not money, is the
reviewer's honest cost - a check that spends it indiscriminately is how the
Client starts waving things through.

So the stopgap is a judgment, not a gate: the Code Reviewer, which already reads
the change, now also says whether each changed test keeps the exam it was
protecting or weakens it. The blanket protection still blocks; the reviewer
supplies what the owner needs to rule on it.

## The three decisions worth remembering

**The verdict is derived, never asserted.** The model judges one changed test at
a time (`judge_test_change`); inspector derives the run's overall verdict from
the record, exactly as the examiner derives per-capability results. Which files
are tests, and whether each was added, deleted, or modified, are read from the
diff by inspector itself (`ChangedTests`, `File.Added`/`File.Deleted`) - the
model cannot argue a test file out of the list.

**Unjudged can never read as intact.** `finish_review` is refused while any
detected changed test carries no judgment, and `cannot_tell` is a first-class
verdict so the model has an honest way out rather than a forced binary. A run
that ends with either derives `test_changes_unjudged`, which names the paths.
Same lesson as the examiner's `could_not_be_tested`.

**An omitted `--test-paths` is named, not silent.** With no patterns the verdict
is `test_change_judgment_not_requested`, printed on every run. "No test changes"
and "nobody told inspector what a test is" are different facts and must not
share a spelling.

## Alternatives rejected

- **Make a weakened test change red, or feed it to the gate.** Rejected: SPEC's
  reviewer is read-only and non-blocking, and SPEC section 8's gate is
  deliberately AI-free - an AI verdict inside the merge decision is the one
  thing that architecture excludes. The exit code stays green for any completed
  review.
- **Infer test files from built-in language patterns** (`*_test.go`,
  `*.test.ts`, `test_*.py`, ...). Rejected: that is the same guess-a-path-from-a-
  string approach this repo already tried and reverted for the check command
  (see `inspector-gate.sh`'s `ALWAYS_PROTECTED` comment), it silently covers
  nothing in a project whose convention is not on the list, and it would be a
  second glob dialect beside the gate's.
- **Read `protectedPaths` out of `.inspector.json`.** Rejected: `inspector
  review` has no repository by design - its whole input is `--diff` - and
  `protectedPaths` means "needs the owner's eyes", which is not the same claim
  as "is a test".

## Sharp edge

`--test-paths` patterns use inspector-gate's glob semantics, not Go's
`path.Match`: `*` matches `/` as well, because the gate matches with bash
`[[ == ]]` and one dialect across both halves is worth more than stdlib reuse.
Bracket expressions and backslashes are refused by name rather than quietly
matched differently from the gate (`internal/reviewer/tests.go`, `compileGlob`).

## Honest limit on how this was built

Issue #44's body could not be read while this was built - the sandbox has no
DNS, no `gh`, and no network of any kind, so the work is written to the issue's
title ("the Code Reviewer judges test changes as a stopgap") plus the problem
this repository had already recorded for itself in `.inspector.json`'s
`protectedPathsNote` and README. If #44 asked for something narrower or wider,
that is where to look first.
