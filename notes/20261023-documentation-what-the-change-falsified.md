# The documentation step asks two questions, and the second one reads files the change never touched

`inspector document` (`internal/documenter`, issue #16) is the documentation
judgment. It exists because a pipeline that already writes documentation well
was still shipping stale documents, and the reason was which question got
asked.

1. **"Is this change described?"** - look at the new code, check the documents
   that cover it. Every documentation step already asks this, and it works.
2. **"Has this change made something already written untrue?"** - go and
   re-read documents the change never touched.

The second is the hard one because **the file that went stale is not in the
diff**. Fabrica PR #74 is the case on record: the change added two entry points
and correctly updated `src/cli/README.md`, while `AGENTS.md` carried a list of
four entry-point names that was now incomplete. `AGENTS.md` was not in the
change, so nothing in the documentation step's field of view included it. The
code reviewer caught it as an informational note and it sat there unfixed until
the Client asked why he was being handed it. Nobody did anything wrong; the
question was never asked.

`internal/documenter/testdata/pr74/` is that shape as a fixture, and
`TestRunAgent_FindsAnIncompleteListInAFileTheChangeNeverTouched` is the
regression: a finding is only accepted there because the corpus, not the diff,
is what inspector let the model read.

## The decisions worth remembering

**Why a new command and not a `--docs` flag on `inspector review`.** The code
reviewer is documented, in its own package comment and in README, as having no
repository mount - its whole input is the caller's diff. Teaching it to read the
project would have falsified its own documentation, in the one piece of work
whose entire subject is that failure. The two judgments also have different
bounds: the reviewer reports anything that looks wrong, while SPEC.md section 4
gives the documentation step two limits (below) that only make sense applied to
documentation. Folding them into one result would leave the Client unable to
separate "this README now lies" from "this code looks wrong."

**Why inspector enumerates the corpus and the model only ever reads from it.**
The obvious implementation is a general file-read tool rooted at the repository.
That hands the choice of what inspector reads to a model that is being fed
project text it cannot vouch for, which turns one careless or hostile document
into a path for reading an untracked `.env` and quoting it back to a model
vendor. So `LoadCorpus` computes the readable set first, from `git ls-files`, and
`read_file`/`search_project`/`list_files` serve only from that set. Tracked
files only - an untracked or ignored file is not a claim the project publishes,
and secrets live exactly there. Non-regular entries are dropped, which also
closes a tracked symlink pointing out of the tree. This is an allowlist computed
before the first model call, the same discipline `container.ResolveGitMounts`
applies to a git directory.

**Why the corpus is all tracked text and not just `*.md`.** Per the Client's
ruling on scope, a claim about behaviour is a claim wherever a person reads it:
prose, doc comments, a sample config, an example command in a script or test.
An extension list would have excluded exactly the in-code cases. What counts as
*text* here is a lighter test than `agentsource.firstNonTextByte`'s - valid
UTF-8 with no NUL - and deliberately so: `agentsource` must refuse anything
unreviewable under the agent tree, while the documenter only needs to skip what
nobody reads as prose, and skipping is not a verdict.

**Why every finding carries a verified quote.** Unlike the code reviewer, which
is handed the whole diff it may cite, the documenter chooses what it reads, so
inspector cannot see the text a finding points at unless the finding brings it.
Each finding must quote the claim verbatim, and `Record.AddFindings` re-reads the
corpus file and requires that quote to appear within two lines of the cited
pointer before it will record anything. A documentation finding whose quoted
claim is not in the file is the precise noise that teaches a reviewer to wave
the whole judgment through.

**How SPEC.md section 4's two limits are enforced rather than requested.**

- *It does not decide the project needs a document it never had.* A finding must
  name a path that is already in the corpus; one naming anything else is
  rejected by name. So the structurally impossible finding is "you should add a
  guide", which is a product judgment and the Client's.
- *The request says what was meant.* `--request` is the only source of intent.
  Whether one was given is recorded on every result as `requestProvided`, so
  "nobody told inspector what was meant" is as visible as the reviewer's
  `test_change_judgment_not_requested` - an absence that reads as approval is
  how this rot started.

**Why a finding can refuse to propose a correction.** Some falsified claims
cannot be rewritten without guessing intent or making a product call. Those
carry `needsClientJudgment` plus the reason instead of a `correction`, and are
printed in their own section. Exactly one of the two is required: a finding with
neither is rejected, because the one outcome nobody wants is silence.

**Why it only reports.** Inspector never edits a judged project (fabrica#95),
so the correction text is a proposal Fabrica applies through the handback loop -
the same shape as the examiner's proposed regression line. The command posts no
commit status and the gate reads nothing new; a completed review exits `0`
whatever it found, exactly as `inspector review` does.

## The same shape, found in this repository

Adding a third caller to `internal/llm` is itself the PR #74 shape, and two
places enumerate that seam's callers:

- `internal/llm/adapter.go`'s package comment said "a caller (the examiner
  today, the reviewer later)" - already stale, since the reviewer shipped with
  issue #34 - and is corrected here.
- `AGENTS.md`'s `internal/llm` bullet says the examiner "was the first caller"
  and the code reviewer "is the second." Both sentences remain literally true
  and neither claims the list is complete, so this is the `needsClientJudgment`
  case rather than a falsified claim: it is left standing and named here on
  purpose, since editing that file's shared body is what makes parallel tasks'
  pull requests collide on one spot.

One further place, pre-existing and untouched: `cmd/inspector/main.go`'s
package comment describes only the project check ("runs a project's own check
command ... and reports green or red"), and has never mentioned `examine` or
`review`. Each new subcommand makes it a little less complete.

## Two limits left standing

- The corpus is read from the working tree as it now stands, and inspector does
  not verify that tree against `--diff`. A caller who hands it a diff of some
  other change gets claims judged against the tree, not against the diff.
- `document` is not wired into `inspector run`. Issue #16 asks for the judgment
  and a case where it finds one; making it part of the gated run would need a
  status, and the Client has not asked for one.
