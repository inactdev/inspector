# The gate's orchestration, under a stubbed gh (issue #21)

Issue [#21](https://github.com/inactdev/inspector/issues/21): `inspector-gate.sh`'s
pure decision functions were well covered and `main()` - the part that sequences
the three GitHub API reads and decides on each answer - was covered by nothing.
PR #19's validation had built a driver that replayed scenarios against a
fixture-serving `gh` stub, off-network, and it passed; the driver was never
committed, so the orchestration was proven once, in a pipeline run's scratch
space. The component deciding whether work can merge had no regression net on
its sequencing.

## The stub is a shell function, not a binary on PATH

`gh_api_call` is the single place this script invokes `gh`, and it invokes it as
a command - so a shell function named `gh` is what it finds. That is the whole
mechanism. No `PATH` is rearranged, which matters for two reasons:

- The drift check at the bottom of `inspector-gate_test.sh` re-runs the whole
  file with `PATH` cut down to exactly `REQUIRED_TOOLS`. A stub script would
  need its own interpreter on that `PATH` just to be found, so the test would
  be adding a tool to the required list to support its own scaffolding.
- A stub binary buys no fidelity. Either shape has to emulate `--jq` and
  `--paginate` itself, because the stub answers with the TSV `main()` would
  have been handed rather than with raw JSON to filter.

Rejected alternative, for the record: extracting the workflow's `run:` block
and driving that, which is the shape PR #19's driver used. It makes the thing
under test a *fetched* copy of the script (base64 in a stubbed Contents
response), not the file on disk, so a change to `main()` would not necessarily
be covered. The workflow's own facts are pinned by reading the YAML as text
instead, which is the precedent `gate-sync_test.sh` already set.

## What the scenarios prove that no pure function could

Which endpoints are read and in what order; that a failed read exits instead of
falling through to a verdict (asserted on the recorded call sequence, so "it
stopped" is a fact rather than an inference from the message); that both commit
statuses come out of ONE statuses call and both config fields out of ONE
contents call; that the base ref arrives whole as a `-f` parameter and never in
the request path, including when it is `release&hotfix`; that the fetched
config's own entries reach the decision; and that question 2 still runs and
still reports when question 1 is already red.

Each claim was checked by breaking the script and watching the right assertion
fail - swapping the statuses list endpoint for the combined one, turning the
failed-read `exit 1` into `failed=1`, pasting the ref into the query string,
deleting the workflow's `TESTS_STATUS_CONTEXT`, re-adding an `on: status`
trigger and a checkout step, and adding a required environment variable nobody
supplies.

## Two things the shape of this test forces

**The captured output is never printed with its `::` intact.** These runs
produce real `::error::` lines, and this file's own stdout is read by GitHub
Actions whenever `script/check` runs there. A fixture's error arriving as a
genuine annotation on inspector's own CI run would be a lie told by the test
suite, so `show_main_out` mangles every `::` before printing.

**`actions/checkout` cannot be checked for by name.** The workflow's header
says "never add `actions/checkout`" in prose, so a substring search finds it
and proves nothing. The assertion is that the workflow uses no action at all -
no `uses:` key anywhere - which is both the actual state and a claim a comment
cannot satisfy.

## The status-event scenario named in the issue no longer exists

The issue asks for "the status-event path" among the scenarios the uncommitted
driver ran. There is nothing left to drive: the `on: status` trigger was built
and removed, because such a run's check attaches to the default branch's tip
rather than the pull request's head and a status event cannot be filtered by
context. What is covered instead is the absence of that trigger, pinned as the
decision it was, plus the thing it was trying to fix - a head commit reads red
while its status has not landed and green on a re-run once it has, with nothing
else changed.

## `cat` and `wc` were missing from REQUIRED_TOOLS

Both are `inspector-gate.sh`'s own dependencies: `gh_api_call` reads its
captured stderr with `cat`, `main()` counts the changed-file listing with `wc`.
Neither was named for as long as nothing exercised `main()`, and the drift test
found them on the new scenarios' first restricted run - which is the argument
for that mechanism over a reviewer's eye, made twice now (`tr` was the first
time). `mktemp` and `rm` left the "cannot be proven, listed outright" group at
the same time: the harness and `gh_api_call` both use them inside the
restricted run now. `ln` and `sed` are still only outer scaffolding.

## Same shape, still uncovered

`gate-sync.sh`'s `main()` and `fabrica-contract-check.sh`'s `main()` are the
same untested-orchestration shape and were left standing - #21 is about the
gate. `gate-sync_test.sh`'s header cited `inspector-gate_test.sh` as precedent
for the limitation, which is no longer true, so it now records the gap as a gap
and says why the same shape was not simply copied over: that script's `main()`
writes rather than reads, so its fixtures have to cover a sequence of mutations
and their idempotence.
