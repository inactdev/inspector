# Recording the test-reviewer's per-file reasons (#55)

The test-reviewer (`cmd/inspector/testchanges.go`, driving
`internal/reviewer`) has always asked the model to judge every changed test
file and to give a reason with each judgment. Until this change only the
failing reasons ever left the process: the `inspector-tests` status
description named the first weakened or unjudged file, and a pass printed one
summary line naming the paths. Everything else - which files, which judgment,
why - was dropped on the floor, and the run report had no field for it, so
the file Fabrica copies onto the task record (`.inspector/latest.json`)
carried no trace of the review at all.

That is unauditable in the one place it matters most. The reviewer is the only
thing that lets a test-touching pull request through the gate without the
Client's own override, and **a model that waves every diff through looks
exactly like a careful one** when the pass records nothing.

## What the report now carries

`inspector.Report` grows a `testReview` object (`*TestReview`, so a run with
nothing to judge carries no empty husk):

    vendor, model   who judged - the same two values the status description names
    state           the status state this review produced
    verdict         inspector's own derived TestChangeVerdict behind that state
    summary         the one line the run printed
    files[]         path, status, judgment, reason - one entry per changed test

Recorded on pass, failure and cannot-tell alike. A file the reviewer never
reached carries an empty `judgment` rather than being left out, because
"nobody judged this" is a fact about the review, not an absence.

## Three decisions worth remembering

1. **The report types are the report package's own, not the reviewer's.**
   `internal/inspector` does not import `internal/reviewer`; `cmd/inspector`
   maps `reviewer.TestChange` onto `inspector.TestReviewFile` instead. The
   alternative - embedding `[]reviewer.TestChange` straight into `Report` -
   is fewer lines, but it points the core report package at the model-facing
   one and pins the on-disk report format to a struct whose JSON tags exist
   for `inspector review`'s own output, so a change made for the model's sake
   would silently change the file Fabrica reads.
2. **It is patched in after the fact, not written with the report.** The
   report is written inside `inspector.Run`, and the judgment cannot move
   before it: it needs the repository's default branch (a GitHub API read)
   and has to run after the check and before the first remote step. So
   `RecordTestReview` read-modify-writes the already-written report and
   `latest.json`, exactly as `RecordPullRequestURL` already did for the pull
   request URL - both now share one `patchReport` helper. A failure to write
   it is a warning on stderr and nothing more: the report is notes, never
   authority, the same rule a failed report save already follows.
3. **The recording happens whether publication completed or not.** The
   judgment runs before the first remote write, so its evidence exists even
   when the staging push or the status post then fails. `cmd/inspector`
   records it right after `publishGreen` returns, before the error is
   handled, rather than inside the publication sequence.

The commit status is deliberately untouched - GitHub caps a status
description at 140 characters, which is why the reasons could never live
there. The status is the signal; the report and stdout are the evidence.

## Related shapes left standing, on purpose

- `weakenedTestReason` / `unjudgedTestReason` still name only the *first*
  offending file, because that is all a 140-character description can hold.
  Every other file's judgment is now in the report and on stdout.
- `inspector examine` and `inspector review` print their full result as JSON
  on stdout, so neither discards the model's reasoning - but neither writes
  it to `.inspector/` either. A caller that does not capture stdout keeps no
  file for them. That is outside this issue's scope and is not changed here.
