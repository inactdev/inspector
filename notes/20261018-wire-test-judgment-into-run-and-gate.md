# Wiring the test-change judgment into `inspector run` and the gate (#44)

The judgment itself landed first (`inspector review --test-paths`, see
`20261017-reviewer-judges-test-changes.md`). This is the half that makes a test
change actually *pass* the gate instead of only being described to the Client.

## Why there is a second status at all

`protectedPaths` carries a blanket `*_test.go` entry in this repository, so
every pull request touching a Go test is red at `gate` until the Client reviews
it and uses his bypass (`protectedPathsNote`, fabrica#96). That hand override is
the biggest daily friction, and it is paid on *every* test change, because the
blunt red says nothing about whether any particular one was honest. SPEC.md
section 5 is explicit that attention, not money, is the reviewer's honest cost.

So the stopgap acts on the judgment in the one place that can act without
putting a model inside the merge decision: a commit status the gate reads.
`.inspector.json` gets a new `testPaths` field - the first one **both halves**
read. `protectedPaths` says what needs the Client's eyes; `testPaths` says which
of those files are tests. Deliberately separate keys: a path can need the
Client's eyes for reasons that have nothing to do with being a test, and
collapsing them would make the exception as wide as the protection.

`inspector run` (`cmd/inspector/testchanges.go`) judges the commit's changed
tests and posts the result as the `inspector-tests` commit status;
`inspector-gate.sh` lets a protected path through when it matches `testPaths`
*and* that status is `success` on the head commit.

## Four decisions worth remembering

1. **The judgment can never fail the run.** Every way it can go wrong becomes a
   `failure` status, never a refusal and never a red exit. A refusal would
   suppress publication, and with nothing published the gate has no status to
   read - which it then correctly treats as red for every changed test anyway,
   so a red exit buys nothing and costs the reader the reason. Blocking lives in
   the status and the gate, the same split `examiner` uses. (The Fabrica
   consumer contract also pins exit codes 0/1/2, so this is not a free choice.)
2. **Ordering.** The model runs *before* the first remote step, so a slow or
   failing vendor leaves no half-published state. The status posts *after* the
   `inspector` status and *before* the branch moves, so the gate's first
   `synchronize` look already sees both - the same reason #18 put the green
   status before the branch move.
3. **The floor outranks `testPaths`.** `ALWAYS_PROTECTED` (`.github/**`,
   `.inspector.json`) is classified `blocked` before `testPaths` is consulted at
   all, so a project cannot declare `.github/**` a test path and hand the gate's
   own definition to a model's judgment. A `testPaths` entry only ever downgrades
   a path the project's own `protectedPaths` made red in the first place.
4. **Absent is not success.** No `testPaths`, or no changed test file, means *no
   status at all* - never a passing one. A passing status in that case would hand
   every project a blanket clearance for protected test files it never asked to
   have judged. On the gate side the same rule runs through the already-tested
   `status_verdict`, so absent and non-success are red there exactly as they are
   for the `inspector` status.

The gate stays AI-free: it compares a status string, the same as it does for the
`inspector` status, and runs no model and no project code.

## Alternatives rejected

- **Make the gate call the judgment itself, or make a weakened test a red exit.**
  Rejected (safest-first): the gate runs on `pull_request_target` with no
  checkout and must stay a string comparison; and a red exit suppresses the
  publication the gate needs, so it would make the pull request *less* legible,
  not more. The commit status is the only seam that carries an AI judgment into
  the merge decision without putting AI in the gate.
- **Derive test patterns from `protectedPaths` instead of adding `testPaths`.**
  Rejected: that would make the exception exactly as wide as the protection -
  every protected path would become clearable by a model's judgment, including
  `script/check`. The two keys mean different things and the narrower one has to
  be declared.
- **Re-read `.inspector.json` in the publication path.** Rejected: the patterns
  ride along on `inspector.Result.TestPaths` from the same config load the check
  itself ran under, so the judgment can never be made against a config the check
  did not run with (and the existing config-less `publishGreen` tests stay
  valid).
- **Two gh API calls, one per status context.** Rejected: both questions are now
  answered from the ONE statuses listing (`state_for_context` picks each context
  out of `context<TAB>state` lines), so a status posted between two reads cannot
  produce a verdict matching neither moment. It also removes the need to
  interpolate a context name into a `gh api --jq` filter, which has no `--arg`.

## Sharp edges

- The judged diff is `merge-base(origin/<default>, C)..C` with `--no-renames`
  (`inspector.CommitDiffAgainstBase`). Renames must stay off: a deleted test
  that git pairs with an added file would otherwise show up as a modification,
  and "the test is gone" is the exact case this is for. The merge base, not the
  base tip, so unrelated base-branch commits never enter the judged change.
- `inspector run` takes `--vendor` and `--model` for this and nothing else.
  Until a caller passes `--model`, a project that declares `testPaths` gets
  `inspector-tests: failure` saying so - which leaves it exactly as red as the
  hand-override status quo rather than passing anything silently, and makes the
  missing wiring loud. Adding `"testPaths": ["*_test.go"]` to this repository's
  own `.inspector.json` is therefore a separate, Client-owned edit: the file is
  itself always-protected, and turning the field on before the runner passes
  `--model` changes nothing about how red this repo's test changes are.
- `inspector-tests` must NOT be added to branch protection's required-checks
  list. Inspector posts no such status when no test file changed, and a required
  status that is never posted never passes. The `gate` job reads it; `gate` is
  the thing that is required.
- `inspector-gate.sh` now requires `TESTS_STATUS_CONTEXT` in its environment
  (`:?`), so a workflow copy that was not updated alongside the script fails
  closed and loudly instead of silently skipping the new question. Adopters copy
  both files, as README already says.
