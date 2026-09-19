# inspector

The shipping gate. It checks that finished work is actually finished, repairs what
it can, and blocks the merge when it can't.

This is the design of record, not a build report - see the Status section of
[README.md](README.md) for what exists today.

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

**inspector** runs on your machine. It does all the work: runs the project's
checks, lints, reviews the change, checks the documentation against it, proves
the claimed outcomes with its own end-to-end tests (section 6), repairs what it
can, re-verifies, stages a green commit, records the result against that commit,
then pushes its pull request branch, opens the pull request, watches CI, and
fixes what CI complains about.

**inspector-gate** runs on GitHub. It is small and dumb on purpose: does this
exact commit carry a green inspector result, and were any protected files
touched. Nothing else.

The split follows the AI. Locally you are already signed in, so checking and
fixing cost nothing extra. In the cloud, an AI would need a copy of that sign-in
stored in repository secrets, and a leaked sign-in is the whole account rather
than a capped amount of money. So the cloud half has no AI in it at all, which
also removes the prompt-injection surface entirely - hostile repository content
has no model to steer.

## 3. When it runs

**On a claim that the work is finished. Never on push.**

This is not about efficiency. inspector has a fixer, and a fixer let loose on
half-written work will confidently repair tests that are failing because the
feature is not written yet. It would patch code mid-change into something nobody
asked for.

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

    2. fabrica hands the branch to inspector          <- the handoff IS the trigger

    3. inspector runs everything - checks, lint, review, docs, outcome tests
       fixes what it may fix, without asking
       stages a green commit, records its result, then pushes its branch
       opens the pull request, watches CI, and fixes what CI complains about

    4a. green -> the delivery reaches the Client -> verdict -> the Client merges
    4b. red -> every finding it may not touch travels with the delivery
        -> the Client rules on all of them at once
        -> verdict fix -> same warm worker, same branch -> back to step 2

Section 5 owns that loop: inspector never stops to ask, and the verdict is the
only decision point.

For a repo with no Fabrica, step 1 is you, and step 2 is you running the command.

**Where the line falls between inspector and a verdict:** inspector handles work
that is mechanically wrong - will not compile, a lint rule, a plainly broken
assertion. A verdict handles work that is *wrong* - it built the wrong thing.

The test: **if fixing it requires knowing what was asked for, it is a verdict. If
it does not, inspector can do it.** A type error needs no knowledge of the
request. A `--json` flag emitting the wrong shape does. Formatting and lint sit
squarely on inspector's side: nothing about them needs to know what was asked
for, and the project cannot be relied on to have run them - across many
repositories some will, some will not, and inspector can guarantee it once.

## 4. Who owns what

**inspector owns everything from "the code is written" to "it is green in CI."**
Running the checks, linting, reviewing the change, checking the documentation
against it, proving the claimed outcomes, repairing what is mechanically broken,
staging a green commit, recording its result, pushing the pull request branch,
opening the pull request, watching CI, fixing what CI complains about, and
blessing the final commit.

**Fabrica owns whether it is the right thing.** Before, by building it. After,
through the Client's verdict and the fix loop back into the same warm worker.

That is the honest form of the separation this document opens with:

> **inspector never judges whether the right thing was built, and fabrica never
> judges whether what it built works.**

Neither one grades itself on the question that decides its own work.

It also settles who reacts to a red build. Whoever repairs must be able to push,
so having fabrica push and inspector repair locally would mean handing patches
back and forth. Inspector owns green publication because inspector repairs. In
v1, a red local result does not create a GitHub branch or status: its local
report returns to Fabrica through a verdict instead. This is a deliberate
publication policy that the Client may overrule, not a claim that red is green
or a refusal.

Running the project's tests in more than one place is not duplication. Fabrica
runs them to know when it is done, the way anyone runs tests while writing code.
inspector runs them as evidence, because the builder's word is not proof. Same
command, different purpose.

### Documentation

inspector reads the change and asks whether the project's documentation still
describes reality, then fixes what has fallen out of step.

It belongs on inspector's side by the same test as everything else here: asking
whether a document still matches the code needs no knowledge of what was asked
for. The code is right there. A README promising behavior the code no longer has
is wrong on its face, and correcting it is repair, not authorship.

Two limits keep it from wandering:

- **Documentation the change made untrue is inspector's to fix.** Documentation
  the project never had is not - deciding a project needs a guide it has never
  had is a judgment about the product, and that belongs to the Client.
- **Where a claimed outcome needs describing, the outcome list says what was
  meant** (section 6). inspector writes the description; it does not invent the
  intent behind it.

This is worth having for the reason that is easy to underrate: nothing else in
the pipeline ever notices documentation rot. Tests do not fail because a README
lies. Left alone it decays quietly until the documents are actively misleading,
which is worse than having none.

### The reviewer

An AI reads the change and says what looks wrong. Not running anything - reading
it, the way a colleague would.

This is the only part that finds what nobody thought to look for. Every other
check here answers a question someone wrote down first: a test asserts what its
author imagined, the outcome list covers what was claimed, lint enforces rules
already agreed. A problem in a path nobody made a claim about is invisible to all
of them.

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

It runs everything to the end, fixes what it may fix without asking, and turns
whatever is left into a red with the reasons attached. It does not pause, does
not queue a question, does not wait. A finding it may not act on is not a
question - it is part of the result.

**The Client's verdict is the one and only decision point.**

    fabrica builds it, hands over

    inspector runs everything - checks, lint, review, docs, outcome tests
      fixes what it may fix, silently
      a red result stays local, with its report and reasons

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

The honest trade: when inspector finishes, the pull request is **not** ready -
it is red with a list. That is the price of not being interrupted, and it is
worth paying.

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

The builder must not write the exam. The Client's ruling, 2026-08-07:

> "I want true separation between the thing building and the thing checking. If
> fabrica creates the checks, then inspector running them won't make any
> difference. Each fabrica PR should ship with a list of expected outcomes
> (specs). Inspector should take those, interpret them however he sees fit, and
> run end to end tests based on those specs to confirm the functionality that
> fabrica claims is there actually exists."

So verification has two layers, and only one of them counts as independent:

- **The project's own checks still run and still gate.** A red suite is a red
  result. But they are the builder's tests - passing them proves the work
  matches the builder's idea of correct, nothing more.
- **The claimed outcomes get inspector's own tests.** The work ships with a
  list of expected outcomes: plain statements of observable behavior ("running
  `x --json` prints the report as JSON"), not implementation ("added a JSON
  serializer"). inspector interprets that list independently and writes
  end-to-end tests from its interpretation - driving the real thing: the real
  app headlessly, the real CLI, the real endpoint. It never reuses the
  builder's tests as proof of the builder's claims.

The verdict is per-outcome - confirmed, not confirmed, or could not be tested -
so a red says which claimed capability is missing, not just "something failed".
The commit status stays red unless every testable outcome is confirmed.

**This list is the statement of intent, not a second thing beside it.**
no-mistakes takes an intent - a sentence saying what the work set out to achieve
rather than a description of the diff - handed to it by whoever starts the run,
precisely so it never has to guess from a worker's transcript. The outcome list
is that same statement, made testable. Do not build both.

For Fabrica, the outcome list rides with the delivery
(github.com/inactdev/fabrica/issues/57), and an outcome Fabrica knows it did
not deliver belongs in `gaps`, never quietly dropped from the list. For a repo
with no Fabrica, a human hand-writes the same list.

Interpretation needs the AI, so this lives entirely in the local half. Nothing
about the cloud gate changes.

## 7. How the gate knows

inspector records its result as a **commit status** - a small record attached to
one exact commit, posted through the API with a token. Not a comment, not a
checklist. Text can be typed by anyone; a status cannot.

For a green result, inspector first stages the commit on the remote, then posts
its status, and only then moves the pull request branch. The gate's first run
therefore sees the result already attached to its exact head commit. A builder
does not publish the branch itself.

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

inspector reads its token from the `GITHUB_TOKEN` environment variable and
posts only for a real green. In v1, red stays local by deliberate policy, and
a refusal posts nothing; both read as failure by the rule above. Posting is not
optional for green: a missing token, a failed staging push, or an API refusal
fails loudly rather than letting a real local green pass silently unrecorded.

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
- **Fabrica has no fixer.** The fixer is inspector's, named for inspector, living
  here. Firstmate has no fixer either - a worker hands off and no-mistakes owns
  everything after. Same shape.

inspector checking a new version of itself is fine; it is what every test suite
does.

## 10. Never merges

Green means ready for a verdict, not merged. Merging is the Client's act.

Auto-merge stays off deliberately. It fires the instant checks pass, which would
let a green check close a task before the Client has ruled on it.

## 11. Deprioritized

**An API-driven fixer.** v1 uses the signed-in CLI, which costs throttling rather
than money when it runs away. A direct API adapter bills a card per token, so it
waits until there is a reason to want it. Recorded as a low-priority issue, not a
gap.

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

**inspector keeps a small local report per run.** The commit status is tiny - it
carries pass or fail and little else, which is enough to gate a merge and not
enough to act on a failure. The report holds the useful part: which claimed
outcome did not hold, what the fixer tried, what the failing output said. When
Fabrica is in the loop that travels with the delivery, but at the day job there
is no delivery to carry it, and a bare red mark would mean "something failed,
re-run it and watch".

The report is notes, never authority. The status on the commit remains the only
thing the gate reads and the only thing that decides a merge.
