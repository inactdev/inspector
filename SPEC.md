# inspector

The shipping gate. It checks that finished work is actually finished and blocks
the merge when it cannot judge it.

This is the design of record, not a build report - see the Status section of
[README.md](README.md) for what exists today.

## Current Client rulings

The sections below preserve the original roadmap's reasoning. The following
later rulings override every contrary statement in them:

- Inspector never edits a judged project. It returns findings and proposed
  regression lines to Fabrica's handback loop (fabrica#95).
- The examiner judges the request, not an implementation or worker-authored
  outcome list. It receives issue text, changed file names, a feature map, an
  always-true list, and pre-task changed tests from Fabrica, then drives a
  running app through a real driver. It derives scenarios from the request
  itself and bounds attempts per task.
- The examiner container never receives application source, an implementation
  diff, or worker-written file content. A changed test name may lead it to a
  pre-task test version, never the worker's version. It posts the separate
  `examiner` status: `failure` means it found a bug; `error` means an otherwise
  incomplete or explicitly refused examination.

Issue #7 and README's Examiner section are the current operational design for
that examiner. They replace the roadmap's earlier outcome-list and fixer designs.

---

## 1. Why it exists

Fabrica reports on its own work.

A Fabrica delivery is a good report - typed rather than prose, with fields a
checkmark cannot carry:

    outcome        done | failure-report | discarded-protected-path
    confidence     a number
    summary        what it did
    evidence       what proves it
    assumptions    what it took for granted
    gaps           what it did NOT do
    branch, files, gateChanges

`gaps` and `confidence` are the parts worth noticing. A green tick can only say
"the steps passed". It cannot say "this passed but I am unsure about the edge
case", or "I did not do the second half". The delivery can, and one structural
guard already holds: `files` is read back from the branch's own diff, so a
delivery cannot list a file it did not change.

**But it is still Fabrica's account of Fabrica's own work.** `evidence` is free
text - "proven" comes down to what a worker chose to write there. The one thing
a self-report can never establish is whether the self-report is true.

That is the whole job. inspector is not a better report from Fabrica. It is
something outside Fabrica that checks the claim independently, using the
project's own checks rather than anyone's description of them.

The same reasoning covers the day job, where there is no Fabrica at all: work
you believe is finished, checked by something that has no stake in believing you.

## 2. Two halves

**inspector** runs on your machine. It runs the project's checks and independently
examines request-derived behavior against a running app. It records both
judgments against the exact commit and reports findings without editing the
judged project. For a green project check, it stages the commit, records the
result, and only then pushes the pull request branch.

**inspector-gate** runs on GitHub. It is small and dumb on purpose: does this
exact commit carry a green inspector result, and were any protected files
touched. Nothing else. Branch protection requires the separate `examiner` status
alongside the gate rather than folding it into the gate's decision.

The split follows the AI. The local half can seal its runtime and allowed inputs
before giving the model credentials or app access. A cloud AI would require
those credentials in repository secrets. Keeping the cloud half free of AI
avoids that exposure and removes the prompt-injection surface entirely - hostile
repository content has no model to steer.

## 3. When it runs

**On a claim that the work is finished. Never on push.**

This is not about efficiency. A judgment on half-written work creates findings
for behavior the builder has not claimed is ready and says nothing about the
finished request.

**Fabrica starts inspector, and nothing else does.** Not a watcher, not a push
hook, not a supervisor telling a worker to run it - the builder hands the work
over when it believes the work is finished, exactly as firstmate hands off to
no-mistakes today. Anything else that could start it is a second trigger with a
second set of assumptions about whether the work is actually done.

Same shape as no-mistakes: it does not watch a branch. Something finishes and
calls it.

    1. fabrica do "<the task>"
       worker writes it, commits as it goes
       fabrica runs the project's check command -> green
       fabrica now believes the feature is finished

    2. fabrica starts the app, then explicitly hands finished work to inspector
                                                          <- the handoff IS the trigger

    3. inspector runs the project check and independent examiner
       records each judgment and every finding without editing the project
       publishes only through each judgment's defined status path

    4a. green -> the delivery reaches the Client -> verdict -> the Client merges
    4b. blocking -> every finding and incomplete result travels with the delivery
        -> the Client rules on all of them at once
        -> verdict fix -> same warm worker, same branch -> back to step 2

Section 5 owns that loop: inspector never stops to ask, and the verdict is the
only decision point.

For a repo with no Fabrica, step 1 is you; when examination applies, you also
start the app before running the command.

Inspector never repairs the judged project. Mechanical failures and
request-dependent findings alike return through Fabrica's handback loop; the
Client's verdict remains the only decision point.

## 4. Who owns what

**Inspector owns independent judgment and the publication steps it performs.**
It runs the project check, drives request-derived app scenarios, preserves the
results against the exact commit, and reports what did not hold.

**Fabrica owns authorship and repair.** It builds the request and applies every
fix after the Client's verdict. Inspector never crosses that boundary, even for
a mechanical edit or a proposed regression.

That is the honest form of the separation this document opens with:

> **Inspector judges Fabrica's work independently; Fabrica never grades its own
> work, and Inspector never rewrites what it judges.**

Neither one grades itself on the question that decides its own work. A red
project check remains local by v1 policy; the examiner posts its separate
blocking status so incompleteness and found bugs stay visible.

Running the project's tests in more than one place is not duplication. Fabrica
runs them to know when it is done, the way anyone runs tests while writing code.
inspector runs them as evidence, because the builder's word is not proof. Same
command, different purpose.

### Documentation

Inspector reads the change and asks whether the project's documentation still
describes reality, then reports what has fallen out of step. Fabrica applies the
correction through the same handback loop as every other finding.

Two limits keep it from wandering:

- **Documentation the change made untrue is Inspector's to report.** Documentation
  the project never had is not - deciding a project needs a guide it has never
  had is a judgment about the product, and that belongs to the Client.
- **The request says what was meant.** Inspector does not infer intent from a
  worker's transcript or implementation.

This is worth having for the reason that is easy to underrate: nothing else in
the pipeline ever notices documentation rot. Tests do not fail because a README
lies. Left alone it decays quietly until the documents are actively misleading,
which is worse than having none.

### The reviewer

An AI reads the change and says what looks wrong. Not running anything - reading
it, the way a colleague would.

This is the only part that finds what nobody thought to look for. Every other
check here answers a question someone wrote down first: a test asserts what its
author imagined, the examiner covers request-derived capabilities, and lint
enforces rules already agreed. A problem in a path nobody made a claim about is
invisible to all of them.

Three real ones from a single day of Fabrica's own development, none of which
failed any test:

- A task could disappear from the record entirely. If the final save failed -
  disk full, a stale lock - the worker's workspace was destroyed anyway and
  nothing was written. The record showed a task that started and then nothing.
- Reopening a task for a fix could attach to a tag that happened to share the
  branch's name, silently orphaning that round's work.
- A repository hook could destroy a worker's uncommitted changes, because the
  instruction to skip hooks did not cover all of them.

**The honest cost is attention, not money.** A reviewer that stops to ask about
every opinion turns into an interrupt generator, and the Client starts waving
things through to make it stop - which is precisely when it becomes decoration.
That is what section 5 exists to prevent.

## 5. The loop

**inspector never stops to ask. It reports.**

It runs every judgment to the end and attaches the reasons to any blocking
result. It does not pause, queue a question, wait, or edit the judged project. A
finding is part of the result, not an intermediate interruption.

**The Client's verdict is the one and only decision point.**

    fabrica builds it, hands over

    inspector checks and examines it without editing the project
      every finding is retained
      a red project check stays local
      the examiner posts its separate blocking status

    Fabrica carries those reasons to the Client for a verdict:
      "fix - do the first two, the third is fine as it is"

    fabrica's warm worker does exactly that
    inspector runs again -> green -> blesses the commit -> the Client merges

**Fixes route to Fabrica, not back into inspector.** By the same test as
everywhere else: if it needed knowing what was asked for, inspector could not
have done it in the first place. Sending it back to inspector would make
inspector both the author and the judge of the same change.

**Why one decision point rather than two.** A version where inspector asks
questions and the Client later gives a verdict has him doing the same act twice
under two names. It also mirrors the failure mode of the tool this replaces:
no-mistakes blocks mid-run, so getting one pull request finished cost the Client
roughly fifteen separate interruptions across a single afternoon. One red
carrying every finding costs him one.

The honest trade: a red project check does not publish the pull request branch.
Its local report carries the reasons instead. The examiner's separate status is
published because found bugs and incomplete examination must remain visible.

### How many times the Client may say fix

**Unlimited, and counted.**

Fabrica has an attempt budget - the number of times it may try on its own before
giving up. That limit exists to stop a machine looping unattended, burning money
with nobody watching. It is right for what it governs.

**A fix the Client asked for is not that.** Nothing happens until he says so, so
he *is* the stop condition and there is no runaway to prevent. Drawing his fixes
from the machine's budget caps him instead of it - and bites hardest in exactly
the case this document describes, where inspector surfaces findings over more
than one round.

So the two are separate. The attempt budget bounds what Fabrica does on its own.
Fix verdicts are bounded by the Client alone, with the round number shown to him
each time - "this is fix round four" is information he can act on, not a wall he
hits. Tracked for Fabrica at github.com/inactdev/fabrica/issues/65.

## 6. Independent verification

The builder must not write the exam. The project's own checks still run and
still gate, but they are builder-authored evidence. Passing them proves only
that the work matches the builder's tests.

The independent examiner derives observable capabilities from the request
itself. It receives a feature map and always-true list for project context,
names-only changed files as signals, and task-starting versions of changed tests.
It receives no worker-authored outcome list, implementation diff, application
source, or worker-written file content. A changed pre-task test contributes the
scenario it protected; a new test with no prior version contributes nothing.

The examiner spends a bounded attempt budget on request-derived scenarios first,
then nearby feature-map and invariant scenarios. The model proposes capabilities,
HTTP attempts, and evidence assessments. Inspector records every attempt and
delivered response per capability and derives each final result itself. No
capability can be confirmed without successful recorded evidence.

The verdict preserves `confirmed`, `not_confirmed`, and
`could_not_be_tested` per capability. It also preserves examination
incompleteness separately, so a confirmed bug cannot hide untested work and
untested work cannot hide a bug. Missing behavior includes a proposed regression
line, never a project edit.

The examiner posts a separate `examiner` status. A bug posts `failure`, including
when the examination is also incomplete. An otherwise incomplete examination or
refusal posts `error`. README's Examiner section owns the operational interface,
input formats, containment boundary, and branch-protection setup.

## 7. How the gate knows

The project check records its green result as an `inspector` **commit status** -
a small record attached to one exact commit, posted through the API with a token.
The independent examiner records its own `examiner` status. Neither is a comment
or checklist. Text can be typed by anyone; a status cannot.

For a green project-check result, inspector first stages the commit on a
non-branch remote ref, then posts its status, and only then moves an explicitly
caller-named pull request branch. It never infers that destination from
checkout, HEAD, tracking state, or another ambient source, and refuses the
remote default branch even
when named. This is a captain decision that may be overruled: ambient authority
conflicts with the explicit handoff architecture, and a freshly pulled checkout
normally points at the default branch. The gate's first run therefore sees the
result already attached to its exact head commit without risking a direct
default-branch update. A builder does not publish the branch itself. On
platforms where inspector cannot guarantee terminating a timed-out push's
complete process tree, it still runs the local check and can return red, but a
green result exits `2` before any remote publication step starts. Before any
remote operation, inspector also confirms that HEAD, the working tree, and the
captured publication target still name exactly the code and destination handed
to the check. It refuses and names tracked changes, non-ignored additions, or
mutable index flags that can hide tracked content, while allowing ignored build
output. Publication pushes run without the checked repository's hooks or local
Git configuration, and Git never receives the status token.
This is a captain decision that may be overruled: a stamp must name the exact
code tested; check-written code exists nowhere and could launder a green result
for a different commit. The v1 guard compares only the post-check snapshot,
however. Because the source mount remains writable, a check can modify a tracked
file, pass against those modified bytes, and restore it before exiting without
detection; a resulting green can describe code that was never the commit
stamped. Issue [#27](https://github.com/inactdev/inspector/issues/27), "Source is
read-only while the check runs," owns closing that gap.

Network failures can lose a successful response, so inspector cannot always
know whether a failed staging push created its ref or a failed final push moved
the named branch. An explicit GitHub rejection confirms that no status was
posted. If the status request was written but its response was lost, inspector
reports the third state **stamp sent, outcome unconfirmed**, distinct from both
confirmed-posted and not-attempted and never presented as green. It identifies
which later operations were not attempted. These incomplete green publications
exit `2`; inspector does not compensate or pretend they are refusals. A
project-check refusal never attempts to push or publish.

- merging stays blocked until inspector has blessed the exact head commit
- a later branch update without inspector's sequence goes red on its own,
  because the new commit has not been inspected

`inspector-gate` asks one question: does this head commit carry a green inspector
result? No result at all is red, the same as a failing one. That is what makes
running inspector non-optional rather than a habit.

The status context is `inspector` - the exact, stable string `inspector-gate`
keys on to find this result among any other statuses on the same commit. Both
halves must agree on this name, since inspector-gate reads it by exact match,
and it is recorded here rather than in either half's code because it is the one
thing both halves need to agree on independently.

The project-check path reads its token from the `GITHUB_TOKEN` environment
variable and posts only for a real green. In v1, red stays local by deliberate
policy, and a project-check refusal posts nothing; both read as failure by the
rule above. Posting is not optional for green: a missing token, a failed staging
push, or an API refusal fails loudly rather than letting a real local green pass
silently unrecorded.

**Honest limit: the token is not an identity boundary.** It cannot tell the
Client from inspector from a worker. All it buys is that a green result cannot be
*typed*. Anything holding the token can post one having checked nothing - and
workers run on the same machine with wide reach, so a worker that cannot pass the
tests could post its own pass. This is the same trust any CI system has, and for
one person it is acceptable.

**So the gate does not take inspector's word for the part that matters.** See
section 8.

## 8. Protected files

Certain files decide whether work passes. Editing them is how a worker fakes a
green result, and it is the most ordinary thing an agent under pressure does -
hit a failing check, make the check stop failing. Not sabotage; the common case.

**Any change to a protected file goes red. There is no declared-changes path that
passes.** The Client releases it deliberately, using his bypass. He was offered a
`Gate-Changes:` declaration that would let a declared change pass green and
declined it: a change to the thing that decides whether work passes deserves his
eyes every single time.

**inspector-gate checks the protected files itself** rather than trusting
inspector's status for it. That comparison is pure code, costs nothing, and needs
no AI. So faking a status can hide a failing test, but it cannot hide an edit to
the gate.

**The gate's own definition must come from the base branch, not the branch under
test.** GitHub runs a workflow from the pull request's own copy by default, which
means a pull request can rewrite the check that judges it - edit the protected
file and empty the protected list in one commit, and the emptied check reports
green. Reading the definition from the base branch removes that entirely.

That mode carries a bad reputation, earned by workflows that use it to check out
and run pull-request code while holding secrets. **inspector-gate must never
check out or execute anything from the branch** - it asks which files changed and
compares names. Adding a checkout would make it unsafe.

**Workflow files are themselves protected**, which only becomes meaningful once
the definition comes from the base branch: the list doing the judging is then one
the pull request cannot rewrite.

## 9. Separate from Fabrica

inspector is its own repository and its own roadmap. It is a tool, not the
product - the same reason firstmate and no-mistakes are not inside Fabrica
either.

- **Fabrica uses inspector.** The dependency points that way. A tool living
  inside the thing it checks is circular.
- **It goes to the day job.** A tool with its own plain name and no connection to
  a personal project is easier to install and explain on someone else's machine.
- **It ships on its own schedule.** Fabrica has not finished Phase 1. inspector
  needs to be usable now.
- **Fabrica repairs Inspector's findings.** Keeping the fixer with the builder
  prevents Inspector from becoming both author and judge of the same work.

inspector checking a new version of itself is fine; it is what every test suite
does.

## 10. Never merges

Green means ready for a verdict, not merged. Merging is the Client's act.

Auto-merge stays off deliberately. It fires the instant checks pass, which would
let a green check close a task before the Client has ruled on it.

## 11. Deprioritized

**Anything that makes protected-file changes smoother.** They should be rare. If
they stop being rare, revisit.

## 12. Settled

All three of the spec's original open questions were ruled on 2026-08-07.

**Identity, not token storage.** The question was where to keep the token so
workers on the same machine cannot read it. That was the wrong shape: a commit
status only proves which *account* posted it, never which *program*, so hiding
the token better never makes inspector's green distinguishable from a worker's.
v1 accepts that - it posts with the Client's own token, and the honest limit in
section 7 stands. The real fix is inspector holding its own identity as a GitHub
App, so the gate can require green *posted by inspector* rather than merely
green; that is issue #9, deliberately deferred.

**A repo with no check command: refuse.** Running unprotected looks identical to
running protected until it matters, so the gap must be loud rather than silent.

**inspector keeps a small local project-check report per run.** The green commit
status is tiny - it carries approval and little else, which is enough to gate a
merge. A red project-check result stays local, where the report holds the
command, failing output, and verdict needed to act without rerunning it. The
examiner instead emits its record-derived JSON verdict and posts its separate
status.

Reports are notes, never authority. A green `inspector` status on the commit, or
its absence, remains the only result inspector-gate reads. Branch protection
requires the separate `examiner` status alongside that gate.
