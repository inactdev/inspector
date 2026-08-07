# inspector

The shipping gate. It checks that finished work is actually finished, repairs what
it can, and blocks the merge when it can't.

Status: specification. Nothing built yet.

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
checks, repairs what it can, re-verifies, and records the result.

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

Same shape as no-mistakes: it does not watch a branch. Something finishes and
calls it.

    1. fabrica do "<the task>"
       worker writes it, commits as it goes
       fabrica runs the project's check command -> green
       fabrica now believes the feature is finished

    2. fabrica hands the branch to inspector          <- the handoff IS the trigger

    3. inspector runs the project's real checks
       repairs what is mechanically broken, re-verifies
       records its result against the final commit

    4a. green -> the delivery reaches the Client -> verdict -> the Client merges
    4b. red after its budget -> its report travels with the delivery
        -> verdict fix -> same warm worker, same branch -> back to step 2

For a repo with no Fabrica, step 1 is you, and step 2 is you running the command.

**Where the line falls between inspector and a verdict:** inspector handles work
that is mechanically wrong - will not compile, a lint rule, a plainly broken
assertion. A verdict handles work that is *wrong* - it built the wrong thing.

The test: **if fixing it requires knowing what was asked for, it is a verdict. If
it does not, inspector can do it.** A type error needs no knowledge of the
request. A `--json` flag emitting the wrong shape does.

## 4. How the gate knows

inspector records its result as a **commit status** - a small record attached to
one exact commit, posted through the API with a token. Not a comment, not a
checklist. Text can be typed by anyone; a status cannot.

Because it is bound to a commit:

- push whenever you like, nothing runs, nothing is watched
- merging stays blocked until inspector has blessed the exact head commit
- push again afterwards and it goes red on its own, because the new commit has
  not been inspected

`inspector-gate` asks one question: does this head commit carry a green inspector
result? No result at all is red, the same as a failing one. That is what makes
running inspector non-optional rather than a habit.

**Honest limit: the token is not an identity boundary.** It cannot tell the
Client from inspector from a worker. All it buys is that a green result cannot be
*typed*. Anything holding the token can post one having checked nothing - and
workers run on the same machine with wide reach, so a worker that cannot pass the
tests could post its own pass. This is the same trust any CI system has, and for
one person it is acceptable.

**So the gate does not take inspector's word for the part that matters.** See
section 5.

## 5. Protected files

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

## 6. Separate from Fabrica

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

## 7. Never merges

Green means ready for a verdict, not merged. Merging is the Client's act.

Auto-merge stays off deliberately. It fires the instant checks pass, which would
let a green check close a task before the Client has ruled on it.

## 8. Deprioritized

**An API-driven fixer.** v1 uses the signed-in CLI, which costs throttling rather
than money when it runs away. A direct API adapter bills a card per token, so it
waits until there is a reason to want it. Recorded as a low-priority issue, not a
gap.

**Anything that makes protected-file changes smoother.** They should be rare. If
they stop being rare, revisit.

## 9. Open

- Where the token lives so that workers on the same machine cannot read it.
- What inspector does on a repo with no check command configured. Refusing is
  probably right - running unprotected looks identical to running protected until
  it matters.
- Whether the local half needs its own record of runs, or whether the commit
  status is the record.
