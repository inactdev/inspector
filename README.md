<img width="1280" height="720" alt="inspector" src="https://github.com/user-attachments/assets/a8685b63-7b08-4102-9d09-bceb811ffc68" />

# inspector

The shipping gate. It checks that finished work is actually finished, returns
findings without editing the judged project, and blocks the merge when it can't
judge the work.

Read [SPEC.md](SPEC.md) first - it explains why this exists, which is the part that
matters. The short version: a tool that reports on its own work can tell you what it
did, what it assumed, and what it skipped, but it can never establish that its own
report is true. inspector is the thing outside that checks the claim, using the
project's own checks rather than anyone's description of them.

It has two halves:

- **inspector** runs on your machine. It runs the project's checks, returns
  findings without editing the judged project, records a green result against the
  exact commit it checked, and only then publishes the explicitly named pull request
  branch.
- **inspector-gate** runs on GitHub. Small and dumb on purpose: does this commit
  carry a green inspector result, and were any protected files touched.

It runs when something claims to be finished, never on push - a judgment on
half-written work would be premature.

It never merges. Green means ready for a verdict, not merged.

## Install

Requires [Go](https://go.dev) 1.22+.

```
go install github.com/inactdev/inspector/cmd/inspector@latest
```

Or build from a checkout:

```
git clone https://github.com/inactdev/inspector.git
cd inspector
go build -o inspector ./cmd/inspector
```

`script/check`, which runs inspector's own tests, additionally needs a C
compiler: it runs the suite under Go's race detector, which requires cgo. The
race detector is itself unsupported on some platforms, including 32-bit x86,
linux/arm, and freebsd/arm - on those, run `CGO_ENABLED=0 go test ./...`
instead, which runs the same suite without the race detector and so needs no C
compiler. `script/check` also runs inspector-gate's own shell tests, which need
a handful of shell tools on PATH; the list itself lives in
`.github/scripts/required-tools.sh` and is not repeated here, because a second
copy only goes stale. `jq` is on it as a dependency of the shipped gate script
rather than only of its tests - it is what inspector-gate.sh parses GitHub's API
responses with. `script/check` checks for every tool on that list before it runs
anything and names whichever one is missing. That is also why inspector's own
`Dockerfile.inspector` builds on `cimg/go:1.22` rather than `golang:1.22`: it
carries all of them together, and the check container has no network to install
anything with.

Also requires [Docker](https://docs.docker.com/get-docker/) - the check
command runs inside a container, not on your machine directly. Without a
usable Docker (installed and its daemon reachable), inspector refuses rather
than falling back to running the check unsandboxed; see below.

## Use

In the repo you want inspected, add two files at the root. `.inspector.json`
names that project's own check command - inspector never guesses at one:

```json
{
  "check": "npm test && npm run lint"
}
```

and `Dockerfile.inspector` is the environment that command runs in:

```dockerfile
FROM node:20
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
```

inspector builds that file at check time and runs the check command in the
result, from `/workspace`, its own working directory, where it finds the commit
under inspection. A project that commits no `Dockerfile.inspector` is refused
by that file's name, the same way a project with no check command is refused -
and so is a build that fails, with docker's log kept.

**The path is fixed and there is no config key for it.** The same image is
meant to be the base of the box the work is *written* in, so what a check runs
against cannot differ from what the work was built against - one file, one
name, in both places.

The built image is tagged `inspector-check:<hash>`, derived from the
Dockerfile's own content together with the image ID each of its `FROM`s
resolves to. Change either and the next run builds a new image; change neither
and it reuses the one already on the machine. Two consequences worth knowing:

- **Pin your base by digest if you want it frozen.** A base image already on
  the machine is not re-pulled, exactly as `docker build` itself behaves
  without `--pull`, so two runs of the same commit can never disagree because
  a floating tag moved between them. `FROM node:20` is resolved to whatever
  `node:20` is here (pulled once if it is missing); `FROM node@sha256:...`
  cannot drift at all.
- **A Dockerfile that copies anything from the repository is rebuilt every
  run** - the tag cannot see the build context, and the whole point of
  `COPY package-lock.json` is that its content changes. Docker's own layer
  cache is what makes that rebuild cheap. A Dockerfile that only installs a
  toolchain (no `COPY`, no `ADD`, no `RUN --mount`) skips the build entirely
  once it has been built once.

Two things inspector refuses rather than guesses at: a `FROM` that names its
image through a variable (`FROM golang:${VERSION}`), because it has to resolve
that base to a digest to tag what it builds, and a `# escape=` parser
directive, because it would be reading the file under different rules than
docker builds it under. Name the base literally.

The build context is the repository root, so a `.dockerignore` is worth having
for the same reason it always is - and worth knowing about: the working tree is
clean by the time a check runs, so tracked content is the commit's, but a
gitignored leftover such as `node_modules` at a path the Dockerfile copies
would reach the image even though it is not part of the commit.

**The check command cannot modify tracked source.** Every top-level path git
tracks - and `.git` itself - is mounted read-only; `/workspace` is otherwise a
throwaway writable directory that belongs to that one run. So a check that
installs packages, builds into `dist/`, or writes coverage and test reports
into its own tree keeps working with nothing configured, while a write to a
tracked file fails with a read-only file system and the check goes red naming
it. The writable workspace is discarded once the run's report is written.

This is enforced where the check runs, not checked afterwards, because
afterwards is too late: a check can save a tracked file, modify it, pass
against the modified bytes and restore it before exiting, and nothing looking
at the tree at the end can see that happened. The capability is removed
instead. The threat is not a hostile project owner - the check command comes
from the project's own repository - it is a worker whose own tests the check
runs, where a test that quietly edits source manufactures a green for code
that never passed as written.

What that does and does not cover, plainly:

- **Covered:** no write to a tracked path succeeds, including creating a new
  file inside a tracked directory - a read-only mount is read-only for its
  whole subtree, and nothing can tell a new file in `src/` from a modified
  one. Nothing the check writes reaches the repository.
- **Not covered:** a check that writes **outside the repository entirely** is
  a different problem, and this is not total containment - the container, its
  resource limits and its denied network are what bound that.
- **Not covered:** a check can still **add** files to its own tree for the
  duration of its own run; they land in the discarded workspace rather than
  the repository, so the post-check addition rule below no longer sees them.
- **Untracked paths that already exist stay writable**, exactly as the whole
  repo was before: gitignored build output, an installed `node_modules` from
  an earlier run, inspector's own report directory. None of them is what the
  commit under check is made of, and keeping them writable is what lets a
  cached install go on being reused.
- **A symbolic link at the repo root is the one tracked path a check can
  still replace**, and the only exception to the first bullet. A link cannot
  be mounted - Docker would resolve it - and resolving an in-repo one would
  hand the container a second real copy of something the commit has once,
  changing what a build walks. So it is recreated as a link, pointing where
  git recorded, with its target read-only; what the check can change is only
  which name points where. A link leading **out of** the repository, or to
  nothing, is refused by name instead of being honored, since honoring it
  would mean mounting a host path the commit does not contain. So is anything
  at the repo root that is neither a directory nor a regular file. Links
  anywhere below the top level are unaffected - they live inside a read-only
  mount.

One other thing is reachable from inside that container, and only when the
repo needs it: its own git directory. An ordinary clone keeps that inside the
repo, so nothing extra is mounted - and it is read-only along with the source,
so a `git commit` in there fails for the same reason. A **linked worktree**
(`git worktree add`)
does not - its `.git` is a one-line pointer file naming the project's shared
git directory by absolute host path, outside the repo entirely, so a check
command that uses git would find no repository at all in there and go red for
a reason that has nothing to do with the code. For that case inspector mounts
the shared git directory **read-only, at the same path the pointer names**, so
git resolves it, `git status`, `git log` and `git diff` work against the
project's real history, and a `git commit` inside the container fails on a
read-only file system.

The one file in a git directory that can carry a credential is its `config` -
a remote URL with an embedded token, a credential helper, an authorization
header, a URL rewrite, an `include` pulling any of those in - and a check
command is arbitrary project code. So the real `config` is never visible
inside the container: inspector shadow-mounts a throwaway copy carrying
forward only the `[core]` section and individually allowlisted, structural
`[extensions]` keys. Inside the container, `git config --get
remote.origin.url` returns nothing. An `[extensions]` key the allowlist does
not cover is refused by name rather than dropped (which would misread the
repository) or forwarded (a future key's value may not be credential-free),
and so is a worktree whose git directory cannot be reached at all: a refusal
and exit `2`, never a red.

**The check container has no network, and there is no way to ask for one.**
`--network none`, every run, no exceptions. That is not incidental: the same
container that keeps a hostile or broken check command from reaching the rest
of your machine is also the thing standing between it and the network, which
is how it would exfiltrate anything it found.

This is what `Dockerfile.inspector` is for. Installing dependencies is the one
thing a check command legitimately needed the network for, and that now happens
at build time, where the network *is* available, rather than at check time:

```json
{
  "check": "npm test && npm run lint"
}
```

```dockerfile
FROM node:20
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
```

A `.inspector.json` that still carries the old `"network"` opt-in is refused
by name, with any value, rather than quietly ignored - as every run that used
it said would happen. So is one that still names an `"image"`: the check runs
in whatever `Dockerfile.inspector` builds, and a config naming a different
image would say something untrue about the run. Delete both keys.

The container also runs as the host user who invoked inspector rather than the
image's own default (commonly root) - `docker run --user <uid>:<gid>` - so a
runaway or hostile check command cannot write into the repo, or anything else
it can reach, as anyone it wasn't already running as. A rootless Docker daemon
is detected and left alone (it already maps its own root to your unprivileged
account). This is also why the git directory trust from above is scoped rather
than wide open: inspector declares `safe.directory` for exactly the paths it
mounted - the repo and, when present, the shared git directory of a linked
worktree - instead of git's wildcard `*`, so running as the matching host user
is reinforced by an allowlist rather than a blanket trust that would cover any
directory, not only the ones inspector chose to mount. If the project's own
image assumes it always runs as root (an entrypoint that `chown`s something, a
tool that refuses to run as an arbitrary uid), this is where that would show up
as a check failure - the fix belongs in the image, not in loosening this.

The container's memory, CPU, and process count are bounded the same way
Fabrica's own worker box is bounded, so a runaway test or a fork bomb in a
project's check command cannot exhaust the host it's checked on. The defaults -
2 GB of memory, 2 CPUs, and 512 processes - are enough for an ordinary test
suite; a project whose checks legitimately need more raises any of them in
`.inspector.json`:

```json
{
  "check": "go test ./...",
  "memory": "4g",
  "cpus": "4",
  "pidsLimit": 1024
}
```

`memory` and `cpus` are passed straight through to docker run's own `--memory`
and `--cpus` flags, in their own syntax; `pidsLimit` is `--pids-limit`, a plain
integer. Omitting any of the three uses the default above, not "unlimited" -
there is no way to ask for unlimited here.

The check command has 15 minutes to finish before inspector kills the
container - including anything it started, like a compound command's children
- and refuses rather than hanging forever, since SPEC.md has Fabrica invoking
inspector unattended. If this project's checks legitimately need longer, set
your own:

```json
{
  "check": "npm test && npm run lint",
  "timeoutSeconds": 1800
}
```

One more field, `protectedPaths`, is read only by inspector-gate, never by
inspector itself - see the "inspector-gate" section below. One further field,
`testPaths`, is read by *both* halves: see
[The `inspector-tests` status](#the-inspector-tests-status).

The order is commit, then inspect. inspector refuses to run against an
uncommitted working tree - it can only be honest about a commit that actually
matches what's on disk. For a green commit C, inspector owns the remaining
publication steps, in this exact order:

1. It runs the check locally on C.
2. If this project declares `testPaths` and C changes a file matching one, it
   judges those changes - before any remote step, so a slow or failing model
   leaves no half-published state behind.
3. It transfers C to a temporary non-branch staging ref on `origin`.
4. It records C's green commit status.
5. If step 2 produced a judgment, it records that as C's `inspector-tests`
   status - before the branch moves, so the gate's first look at the pull
   request already sees both.
6. It updates the explicitly named pull request branch on `origin`,
   fast-forward only unless `--expected-tip` authorized replacing the history
   that branch currently has.
7. If both `-pr-title` and `-pr-body-file` were given, it opens a pull
   request for that branch, or reports the existing one. This step is
   optional and always comes after the branch has already moved - a failure
   here is reported as its own warning, never as a red verdict or an
   incomplete publication, because the check and its publication already
   succeeded by the time it runs.

Name that destination on every invocation:

```
inspector --branch my-feature
```

This command is the publication handoff, not a check to run after another tool
has pushed. A builder must leave C local and must not push C or open its pull
request first. On exit `0`, Inspector has moved the named branch. Pull request
creation remains part of Inspector's publication flow, never work the builder
may perform on Inspector's behalf. On any other exit, nothing else may publish
that commit. The named branch may contain earlier work, but it must not already
point at C. Inspector refuses to stamp C retroactively because that cannot prove
the required stage, status, then branch-update order.

When changing Inspector's publication protocol itself, run the command from the
clean checkout being tested instead of an older installed binary:

```
GITHUB_TOKEN="$(gh auth token)" go run ./cmd/inspector --branch my-feature
```

That self-hosting handoff is required for this repository too. A general build
or delivery pipeline that pushes its commit directly will create a pull request
head with no `inspector` status, and `inspector-gate` will correctly reject it.
Running Inspector afterward is not a recovery path: Inspector refuses to
retroactively approve a commit that the named branch already exposes. Recovery
requires a new local correction commit, handed to Inspector before any tool
publishes it.

`--branch` is required. Inspector never derives publication authority from the
checkout, HEAD, tracking configuration, or any other ambient state, and it
refuses the remote default branch even when explicitly named. This is a captain
decision that may be overruled: ambient destination authority conflicts with
the architecture's explicit handoff, and a freshly pulled checkout is normally
on the default branch, where an inferred push would bypass the pull request.

#### Publishing a rebased branch: `--expected-tip`

The branch update in step 6 is fast-forward only. That is the right default,
and it is also why a branch whose history was rewritten locally - rebased onto
a moved base, for instance - cannot be published: C does not contain what the
remote branch already holds, so the push is rejected and the run ends as an
incomplete publication, with C's green status recorded and the branch left
behind.

`--expected-tip <sha>` is how a caller authorizes that rewrite, one run at a
time:

```
inspector --branch my-feature --expected-tip 0b5a1c9e4d3f27a86b40ff51c2d9e7a3418bc6d2
```

It names the full commit SHA the branch must still point at on `origin`.
Inspector reads the remote branch before anything is pushed and refuses, naming
both commits, if it is anywhere else - or if it does not exist at all. The push
itself then carries a lease on that same commit, so if the branch moves in the
window between that read and that push, the remote refuses it and the branch
keeps the other commit. A rejected lease is one of the few remote outcomes
inspector can report as certain rather than unknown: the remote answered, so
the branch did not move.

The flag takes a complete 40- or 64-character lowercase hexadecimal object
name. An abbreviation, a branch name, or `HEAD~1` is refused before the check
runs - a lease is only worth having if the commit it protects is unambiguous.

Naming the right tip never costs anything: an ordinary fast-forward satisfies
the lease too, so a caller that knows the branch's remote tip may pass it on
every run rather than only after a rebase.

Omit it and nothing changes: the branch update stays fast-forward only, and a
caller that never rebases never needs the flag. The remote is deliberately not
read for the caller, since a lease against whatever inspector happened to
observe seconds earlier would quietly overwrite work nobody accounted for,
including on a mistyped `--branch`.

The staging ref exists only so GitHub has C when it receives the status. After
the named branch moves, Inspector attempts to remove the staging ref and warns
if it cannot confirm removal. Inspector captures `origin`'s publication target
before the check and confirms repository metadata still names that exact target
afterward. It also confirms C is still HEAD and rechecks the working tree
before any remote operation. It refuses and names every tracked change,
non-ignored addition, or mutable index flag that could hide tracked content;
ignored build output does not block publication. This is a captain decision that
may be overruled: a stamp must name the exact code tested, while a check-written
change means the passing code exists nowhere and could launder a green result
for a different commit.

This recheck compares only the post-check snapshot, which is why it is not
what keeps the source honest. A snapshot cannot see a tracked file that was
modified and put back before the run ended, so the source mount is read-only
instead and the capability is gone - issue
[#27](https://github.com/inactdev/inspector/issues/27), "Source is read-only
while the check runs," above. The recheck is kept anyway and unchanged: it
binds the result to one exact commit, which is a different job from
immutability - HEAD moving, a `git commit` from elsewhere on the machine, or
the publication target being retargeted mid-run are all things no mount flag
would catch. Its one weakened reach is noted above: a file the check adds to
its own tree now lands in the discarded workspace, where this rule does not
look for it.

The pull request branch never points at C until its status already exists, so
inspector-gate's first run sees the result instead of a stale missing-status
failure.

**v1 red policy, which the Client may overrule:** a red result stays local.
inspector writes its report and returns `1`, but neither pushes a branch nor
posts a status for work it did not approve. This preserves the boundary that
unverified work does not become a GitHub branch. The local report is the record
for deciding what to do next.

```
inspector --branch my-feature
```

This runs the configured check command against the repo's current HEAD and
prints green or red, always naming the exact commit it inspected - that's how
a caller compares what inspector blessed against what it expects.

Exit codes reserve `0`, `1`, and `2` for verdict attempts:

- `0` green
- `1` red
- `2` no verdict, with one of two visibly different causes:
  - **refused** - inspector stopped before attempting any remote publication.
    This includes local-check failures such as missing configuration, an unusable
    container runtime, a dirty working tree, a repo root inspector cannot mount
    read-only, a timeout, or infrastructure
    failure, and green-publication preflight failures such as an unsupported
    platform, missing token, invalid remote, default-branch target, or a
    `--expected-tip` the remote branch is not at. A refusal
    never attempts a staging push, status publication, or branch update.
  - **incomplete green publication** - the local check passed and publication
    began with the staging push, but inspector could not confirm the complete
    staging, status, and branch sequence. The attempt may have left the staging
    ref, status, or branch state described below.

  Neither cause is red. Never treat `2` as red.

  A signal kill is detected two ways, because a compound check command like
  `npm test && npm run lint` doesn't show it the same way a plain one does:
  `sh -c "a && b"` forks a child for each command and waits on it, so if `a`
  is killed by a signal, `sh` itself is never signaled - it sees its child's
  wait status and exits normally with `128 + <signal number>`, the shell's own
  convention for reporting exactly that. inspector treats *either* shape (the
  shell itself signaled, or an exit code of 128 or above) as refused, not red.
  A program could in principle choose an exit code in that range for its own
  reasons and get called refused when it actually failed - but that direction
  is safe, since refused still blocks the merge, while the alternative sends
  someone hunting a bug that was never there when the check simply ran out of
  memory.
- `64` usage - not a verdict attempt at all: `--help`, an unrecognized flag, or
  bad usage. Distinct from `0`/`1`/`2` so a caller can never mistake a help
  request for a result; `64` follows the BSD
  [sysexits.h](https://man.freebsd.org/cgi/man.cgi?query=sysexits) convention
  for a command-line usage error.

Flags:

- `--branch <name>` - required pull request branch to publish after green
- `--expected-tip <sha>` - optional; the full commit SHA that branch must still
  point at on `origin`, which authorizes replacing a history it already holds
  (see [Publishing a rebased branch](#publishing-a-rebased-branch---expected-tip)).
  Omitted, the branch update stays fast-forward only.
- `--repo <path>` - inspect a repo other than the current directory
- `-pr-title <text>` and `-pr-body-file <path>` - optional; given together,
  open a pull request for `--branch` against the repository's default branch
  once a green result is published, or report the existing one if that
  branch already has an open pull request. Given alone, either flag is a
  usage error, refused at flag-parsing time before any check runs or
  anything is pushed. Given neither, no pull request is opened and every
  existing caller is unaffected. On success inspector prints `pull request:
  <url>` on its own line and records the URL in the local report JSON. Red
  and refused results never open a pull request.

Anything after the flags is a free-text claim, recorded for context and not acted
on:

```
inspector --branch my-feature implemented the login flow
```

Each run writes a small JSON report to `.inspector/runs/`, and copies it to
`.inspector/latest.json`, so a red result is actionable without re-running: the
command that ran, its exit code (or the signal that killed it), and its full
output. A run that judged the commit's changed tests also carries a
`testReview` object - the vendor and model that judged, the status state and
inspector's own derived verdict, and one entry per changed test naming its
`path`, `status`, `judgment` and the reviewer's `reason`. See
[The `inspector-tests` status](#the-inspector-tests-status). These are notes for
a human, never authority. On its first run inspector
writes `.inspector/.gitignore` containing `*`, so reports stay out of git without
you editing anything; an existing `.inspector/.gitignore` is left alone.

Because the report is notes and not authority, a report that can't be saved
never changes the local check result. Inspector prints a loud warning to stderr
naming where the save failed, then still returns red or proceeds with green
publication. A report failure alone does not cause a refusal.

### Recording the result

A green result gets posted to GitHub as a **commit status** on the exact
commit inspected, under the status context `inspector` - a small record
attached to that commit, not a comment or a checklist, because text can be
typed by anyone and a status cannot. It carries pass and a short description
pointing at `.inspector/latest.json`; the report holds the detail, the status
just says whether to trust it.

This needs a token: set `GITHUB_TOKEN` to one with commit-status write access
on the repo, and `origin`'s push URL must point at the same GitHub repository
that receives the status. HTTP push URLs must not contain embedded credentials,
which would expose them in the push process arguments; use a credential helper
or SSH instead. Inspector pushes from a temporary bare repository that can read
C's objects but cannot inherit the checked repository's hooks or local Git
configuration, and it does not pass `GITHUB_TOKEN` to Git. Pushes are
non-interactive and time out rather than waiting forever for credentials or a
stalled transport; a timeout kills git and its helper processes together. On
platforms where Inspector cannot guarantee that complete process-tree
termination, it still runs the local check and can return red, but refuses a
green publication before starting any remote step.
Posting is not optional - a missing token, an unresolvable remote, a failed
staging push, or GitHub refusing the request all fail loudly and exit `2`, even
when the local check passed. A real local green that inspector could not
publish in full is worth nothing to a reader who can only see GitHub, so it
must never look like success.

Once publication starts, inspector does not try to erase partial remote state.
A failed network operation can mean either that GitHub rejected the operation or
that GitHub completed it but the client lost the response. Inspector therefore
reports uncertain remote state honestly: a failed staging push may have created
the staging ref, and a failed final push may have moved the named branch. The
one exception is a `--expected-tip` lease the remote refused, which proves the
remote answered and the branch did not move; inspector reports that as certain.
An
explicit GitHub rejection confirms that no status was posted. If the status
request was written but its response was lost, inspector reports the distinct
state **stamp sent, outcome unconfirmed** - never green, never silent, and
separate from both confirmed-posted and not-attempted. It also states which later
operations were never attempted. These incomplete publications are distinct
from a refusal and exit `2`. This is the captain's deliberate policy and may be
overruled: compensation cannot make a partially published sequence atomic.

What a reader should conclude:

- **Green status** - inspector ran this exact commit's checks and they passed.
- **Red status** - a manually recorded failure. v1 inspector does not publish
  red work or post this status itself.
- **No confirmed status** - this commit has not been approved, or inspector
  could not confirm its green stamp. Read the same as red. A status from an
  earlier commit does not carry forward.

The token is not an identity boundary - see SPEC.md section 7 for the honest
limit on what a green status does and doesn't prove.

#### The `inspector-tests` status

When a project declares `testPaths` in `.inspector.json` and the checked commit
changes a file matching one of them, inspector posts a **second** commit status
on the same commit, under the context `inspector-tests`. It carries one
question's answer, judged by the reviewer's own engine (see
[Judging test changes](#judging-test-changes)) through the same `internal/llm`
vendor seam and the same environment-read key `inspector review` uses:

| state | meaning |
| --- | --- |
| `success` | every changed test file was judged to still prove what it proved before |
| `failure` | one of them weakens the exam, or could not be judged at all - the reason is in the description |
| *(not posted)* | this commit changed no file matching `testPaths`, or the project declares none |

```
{
  "check": "script/check",
  "protectedPaths": ["script/check", "*_test.go"],
  "testPaths": ["*_test.go"]
}
```

`testPaths` uses the same glob dialect as `protectedPaths`, and both halves of
inspector read it: the local CLI to know what to judge, and `inspector-gate` to
know which protected path this status is allowed to clear. Declaring it is what
turns the status on; a project without it behaves exactly as it did before the
status existed.

Judging needs a model, so `inspector run` takes `--vendor` and `--model` for it
- the same two flags, the same defaults, and the same
`<VENDOR>_API_KEY`/`<VENDOR>_BASE_URL` environment reads as `inspector review`.
The vendor is named in the status description, never left silent.

The status is the signal; the run report is the evidence. GitHub truncates a
status description at 140 characters, so the status names at most one file.
Every per-file judgment and the reviewer's reason for it - **on a pass exactly
as on a failure or a `cannot_tell`** - is recorded in the run report's
`testReview` object and printed on stdout under the one-line summary:

```
inspector-tests: success - every changed test keeps its exam intact (feature/rounding_test.go)
  judged by vendor: anthropic (default, no flag given), model: claude-sonnet-4-5
  feature/rounding_test.go (added): keeps_the_exam_intact - new coverage for rounding; it removes nothing the request asked for
```

A pass that recorded no reasoning would be unauditable: the reviewer is the one
thing that lets a test-touching pull request through the gate without the
Client's own override, and a model that waves every diff through looks
identical to a careful one. A changed test the reviewer never reached is listed
with no judgment rather than left out, and a report that cannot be updated with
the review is a warning only - the report is notes, never authority.

Every way this can go wrong **fails closed**: a missing `--model`, an
unreadable diff, an unusable pattern, a vendor that cannot be reached, a
weakened exam, and an unjudged change all post `failure` with the reason, so no
changed test ever clears the gate on silence. What it deliberately does **not**
do is change the run's verdict or exit code. A failing `inspector-tests` status
on a green run still exits `0`, because a red exit would suppress publication
entirely and the gate would then have no status to read - blocking lives in the
status and the gate, exactly as it does for `examiner`. Inspector prints the
failure loudly on stderr so a green line can never be mistaken for a merge.

## Reviewer

`inspector review` is the independent code reviewer (SPEC.md's "The reviewer").
It reads a change and reports anything that looks wrong, each finding pointing
at an exact file and line - the way a colleague reading the diff would. It
never runs the reviewed application and never edits the reviewed project, and
it finds what nobody wrote a test or a feature map for: the only check here
that answers a question nobody asked first.

```
ANTHROPIC_API_KEY=... inspector review --diff /path/to/change.diff --model claude-sonnet-4-5
```

`--diff` is a unified diff of the change (`git diff` output); inspector never
computes it from a repository mount. Each line inspector shows the model is
annotated with the line number a finding must cite - the same number the new
file will actually have - so a finding's pointer never depends on the model's
own diff arithmetic. A finding naming a file outside the diff, or missing a
line or summary, is rejected rather than recorded.

`--vendor` selects the model vendor through the same `internal/llm` seam the
examiner uses (see below), so the reviewer can be told a vendor different from
whichever one wrote the change. Omit it and inspector uses `anthropic`, exactly
as `inspector examine` does; the vendor is always named explicitly, never
silently, in the printed result.

The reviewer reports; it never gates a merge or posts a commit status. Like
every inspector finding, what to do about one is the Client's call, carried
back through Fabrica's handback loop - not a decision inspector makes for him.

### Judging test changes

The cheapest way to make a failing check stop failing is to change the exam
rather than the code, and it is the most ordinary thing a builder under pressure
does (SPEC.md section 8). `--test-paths` tells the reviewer which files in the
change are tests, and it then judges each one:

```
ANTHROPIC_API_KEY=... inspector review --diff change.diff --model claude-sonnet-4-5 \
  --test-paths '*_test.go'
```

One comma-separated list of glob patterns, matched with inspector-gate's
semantics rather than Go's: `*` matches `/` as well, so `internal/*_test.go`
covers every depth under `internal/`. A pattern using a bracket expression or a
backslash is refused by name - those mean something in the gate's matcher that
this one does not reproduce, and a pattern that matches differently in the two
halves is worse than one you are told to rewrite.

Inspector decides which files are tests and whether each was added, deleted, or
modified, reading both from the diff; the model judges only what the change did
to the exam, one file at a time, and cannot finish the review while any of them
is unjudged. Inspector then derives the run's single verdict:

| verdict | meaning |
| --- | --- |
| `test_change_judgment_not_requested` | no `--test-paths` was given, so no test change was judged - printed on every such run |
| `no_test_changes` | patterns were given and the change touched no file matching them |
| `test_changes_keep_the_exam_intact` | every changed test still proves at least what it proved before |
| `test_changes_weaken_the_exam` | at least one change removes, loosens, skips, or narrows what its test proved |
| `test_changes_unjudged` | at least one changed test carries no judgment or an explicit `cannot_tell`, and none was judged weakened |

An unjudged test change can never read as an intact one, and a model that
answers `cannot_tell` lands here rather than being forced into a guess - the
same separation the examiner keeps between a found bug and untested work. A
second judgment for a path already judged is rejected rather than replacing the
first, so a weakened verdict cannot be written over with an intact one.

`inspector review` itself only reports: its exit code stays green for any
completed review, and a weakened exam travels with the delivery as a finding
for the Client to rule on. What acts on the same judgment is `inspector run`,
which posts it as the [`inspector-tests` status](#the-inspector-tests-status)
and lets `inspector-gate` clear a changed test file on it. That keeps SPEC.md
section 8's gate AI-free: the gate reads a status string, the same way it reads
the `inspector` one, and runs no model and no project code itself.

Until the behavior tester can judge test changes, this is the stopgap that
makes the blanket `*_test.go` entry under `protectedPaths` livable: without it
every pull request touching a test is red by design and needs the Client's
override by hand
([fabrica#96](https://github.com/inactdev/fabrica/issues/96)). It is a stopgap
for exactly as long as that entry is.

## Documentation

`inspector document` is the documentation judgment (SPEC.md section 4's
"Documentation"). It asks two questions about a change, not one:

1. **Is this change described?** Look at what the change did and check the
   documents that already cover that ground.
2. **Has this change made something already written untrue?**

Every documentation step asks the first one. The second is the one that gets
missed, and it is why this exists: **the file that went stale is not in the
diff.** A list elsewhere that no longer lists everything. A README promising
behavior that moved. A described default, limit, or exit code that changed. An
example command that no longer runs. Nothing else in the pipeline notices -
no test fails because a document lies - and it compounds, because each stale
claim makes the next reader trust the documents slightly less.

```
ANTHROPIC_API_KEY=... inspector document --diff change.diff --repo . \
  --model claude-sonnet-4-5 --request-file request.txt
```

`--diff` is a unified diff of the change, annotated with line numbers the same
way `inspector review` annotates its own. It answers *what changed*; it is
never where the stale document is found. `--repo` is the project whose
documentation to re-read, and `--request-file` is the request or
expected-outcome text.

### What it is allowed to read

Inspector builds the corpus itself, before the first model call, from the
project's **tracked text files** - then gives the model three tools that serve
only from it: a literal case-insensitive search, a line-annotated read of one
file, and a path listing. The model never picks which bytes inspector opens.

That is deliberate. The obvious implementation is a general file-read tool
rooted at the repository, which hands that choice to a model being fed project
text nobody can vouch for - one careless or hostile document away from reading
an untracked `.env` and quoting it back to a model vendor. Tracked files only,
because an untracked or ignored file is not a claim the project publishes and
is exactly where a secret lives. Non-regular entries are dropped too, which
also means a tracked symlink cannot point the corpus out of the tree.

The corpus is **not** limited to `.md` files: a claim about behavior is a claim
wherever a person reads it - prose, doc comments, inline comments, sample
configuration, an example command in a script or a test. Whatever inspector
could not read is counted and named rather than silently dropped, so a
judgment made over a partial corpus never prints like one made over the whole
project.

### What a finding has to carry

| field | what it is |
| --- | --- |
| `kind` | `falsified_claim` (question 2) or `undescribed_change` (question 1) |
| `path`, `line` | where the claim is, in the corpus |
| `claim` | the text **verbatim** as the file has it |
| `summary` | what the change did that made it untrue, or what is missing |
| `correction` | proposed replacement text - or `needsClientJudgment` instead |
| `inChange` | inspector's own fact: was this file part of the change? |

Unlike the code reviewer, which is handed the whole diff it may cite, this
judgment chooses what it reads - so inspector re-reads the corpus file and
requires the quoted claim to be there, within two lines of the cited pointer,
before recording anything. A finding whose claim is not in the file is
rejected, not recorded. `inChange: false` is the case issue #16 exists for, and
it is printed as *"this file is NOT in the change"* rather than left for a
reader to work out.

Then inspector derives the run's single verdict:

| verdict | meaning |
| --- | --- |
| `documentation_consistent` | the review completed and the documents still describe reality |
| `documentation_undescribed_change` | the change is not described somewhere it belongs, and no existing claim was found falsified |
| `documentation_falsified_claims` | at least one thing already written is no longer true - this outranks the others |

### The two limits it does not cross

Both come from SPEC.md section 4, and both are enforced rather than requested:

- **It reports on documentation that already exists.** A finding must name a
  file already in the corpus, so the structurally impossible finding is "the
  project should add a guide" - that is a judgment about the product and it
  belongs to the Client.
- **The request says what was meant.** `--request-file` is the only source of
  intent; intent is never read out of the implementation. Whether one was given
  is printed on *every* run, so "nobody told inspector what was meant" is as
  visible as the reviewer's `test_change_judgment_not_requested`.

A claim that looks falsified but cannot be corrected without a product decision
carries `needsClientJudgment` and its reason instead of a correction, and is
repeated under its own heading on stderr. Never silent, never a guess.

Like `inspector review`, this judgment **only reports**: a correction is
proposed text Fabrica applies after the Client's verdict, never an edit
inspector makes (fabrica#95). It posts no commit status and gates no merge, and
a completed review exits `0` whatever it found. One honest limit: the corpus is
read from the working tree as it now stands, and inspector does not check that
tree against `--diff`.

## Guidebook

`inspector guidebook` writes the examiner's operating instructions for a
project: what the app's features are, how to start it, how to drive each
feature, how to observe the result, and what cannot be tested here.

It exists because of the gap the examiner's own restriction creates. The
examiner is given a running app and no source, which is the whole mechanism -
and an AI that cannot read the code also cannot discover from the code how to
start the app, reach a screen, or call an endpoint. Without something teaching
it, a fresh-eyes judge either guesses or reports "could not be tested" on
everything. So this command has exactly the access the examiner is denied, and
converts it into operating instructions once, at setup time. Same layer as the
installer (issue #5): something that knows the concrete project and writes
something a human sees and accepts.

```
ANTHROPIC_API_KEY=... inspector guidebook --repo /path/to/project --model claude-sonnet-4-5
```

**Facts first, and the facts stay in the output.** Inspector extracts the
project's deterministic facts itself - the check command and image from
`.inspector.json`, manifests, run scripts, compose services, published ports,
Dockerfile directives, HTTP route registrations, CLI entry points, command
flags, environment variables, listen addresses - each with the file and line it
came from. The model then writes the prose over those facts and must cite them
by id; inspector rejects an id it did not extract, and rejects a feature or a
`Launch` section resting on nothing. The finished document ends with every fact
listed verbatim, so a reader can tell what was read from what was concluded.

That half needs no credential and writes nothing, which makes it worth running
on its own:

```
inspector guidebook --facts-only --repo /path/to/project
```

**Inspector renders the document; the model never lays it out.** The file is
`INSPECTOR-GUIDEBOOK.md` at the project root, beside `.inspector.json`,
committed, with fixed sections in a fixed order - `Launch`, `Doctor`, `Drive`,
`Evidence`, `Cleanup`, `Cannot be tested here` - then `Features`, one entry per
user-facing feature with exactly four headings: sub-features, how to get to it
from the user's point of view, driving it with the harness, and gotchas. A
missing section is a refusal rather than a quiet gap, because each one answers a
question the examiner would otherwise guess at. `Launch` and `Doctor` must carry
a real fenced command: somebody executes those two before any judgment happens,
and "start it the usual way" has never started anything.

**Running it twice never costs you a confirmed guidebook.** The real file is
created with `O_EXCL` and is never written over. When it already exists, this
run leaves it exactly as it is and writes `INSPECTOR-GUIDEBOOK.draft.md` beside
it for a person to diff; an earlier draft is replaced, and the command says so.
A guidebook on disk may have been read, corrected and signed by hand, and a
generator that discards that teaches everyone to stop confirming.

**Nothing it writes is authority.** A generated guidebook's front matter says
`confirmed: no` until a person follows every command in it and replaces that
line with `confirmed: YYYY-MM-DD Their Name`. Until then the examiner records
every capability driven from it as `could_not_be_tested` - see the Examiner
section. The command says this loudly on stderr, because a guidebook mistaken
for authority is the failure this whole shape exists to prevent.

It posts no commit status and gates no merge, exits `0` when it wrote a
guidebook and `2` when it refused, and never regenerates anything during an
examination: generating reads the repository, examining does not, and the two
are different commands.

[examples/inspector/](examples/inspector/) is a generated guidebook for this
repository - deliberately the hard case, since inspector has no HTTP interface
at all - together with the trial that followed every command in it and the three
things that trial corrected.

## Examiner

`inspector examine` is the independent outcome judge. It derives scenarios from
the request, operates an already-running HTTP app, and never reads the judged
application's implementation or worker-written test content.

The first version drives HTTP backends. It does not drive native iOS screens, so
an Inkwell examination covers its backend only. The hand-written first
[Inkwell feature map](examples/inkwell/EXAMINER_GUIDEBOOK.md) and
[always-true list](examples/inkwell/inkwell-always.json) are worked examples.
The Guidebook section below owns generating a feature map for any project
(issue #22). The always-true list is the same file check 3 drives - see the
Always-true list section below - so `--always-true` takes that list rather
than a document of its own.

`--feature-map` may be either a hand-written document like Inkwell's or a
generated guidebook. A generated one carries a `confirmed` line, and the
examiner reads it: until that line names a date and a person, every capability
it would have confirmed is recorded as `could_not_be_tested`, the examination is
marked incomplete, and the status is `error`. A found bug stays `failure` -
unreviewed instructions are a reason to doubt a pass, never a reason to clear a
failure. Which of the three states applied is printed as a `guidebook:` line and
carried in the JSON verdict, so an examination that confirmed nothing because
nobody signed its guidebook never reads like one that found the app wanting.

Build the trusted runtime once, before examining. This build is local; the
examination command never builds or pulls an image while it has app access and
an API key:

```
internal/examiner/runtime/build.sh
```

The runtime Dockerfile compiles the reviewed agent source with a pinned,
multi-architecture Go builder into a digest-pinned Linux base. It works from a
Linux, macOS, or Windows host without mounting a caller-supplied executable.
The image is intentionally general-purpose so a future browser driver can live
there too.

No file under the agent source tree - `cmd/examiner-agent/`,
`internal/examiner/`, `internal/container/` and `internal/llm/` - may contain
binary content, because a committed executable or archive is unreviewable by
construction and this agent's whole job is refusing to trust what it cannot
verify; `internal/agentsource` enforces that from the project's own check
command, reads bytes rather than file names, and admits a genuinely binary test
fixture only through an explicit, individually reasoned allowance under a
`testdata` directory.

The caller starts the app outside the examiner, then provides the permitted
input files explicitly:

```
ANTHROPIC_API_KEY=... GITHUB_TOKEN=... inspector examine \
  --request /path/to/request.md \
  --feature-map /path/to/feature-map.md \
  --always-true /path/to/always-true.md \
  --changed-files /path/to/changed-files.json \
  --base-tests /path/to/base-tests.json \
  --app-url http://host.docker.internal:8080 \
  --network bridge \
  --budget 12 \
  --commit <40-character-commit-sha> \
  --owner inactdev \
  --repo inkwell \
  --model claude-sonnet-4-5
```

`--request` is the issue or pull request text. `--feature-map` teaches the
examiner how to drive the app, and `--always-true` names its invariants. The
per-task `--budget` bounds app-driving attempts: request scenarios come first,
then nearby feature-map and invariant scenarios, then the examiner stops.

`--vendor` picks which model vendor the examiner asks, through one small
seam (`internal/llm`) shared with every other AI-using part of Inspector.
Omit it and Inspector uses `anthropic`, exactly as before. Naming any other
vendor - `xai`, `openrouter`, a local `ollama` - switches to an
OpenAI-compatible adapter that works against any endpoint speaking that
request shape, never one vendor only. Secrets reach the adapter by name from
the environment, never on the command line, under one fixed rule with no
per-vendor table: uppercase the vendor name and append `_API_KEY`, and for
every vendor except `anthropic` also `_BASE_URL`. `--vendor openrouter` reads
`OPENROUTER_API_KEY` and `OPENROUTER_BASE_URL`; a missing one refuses, naming
the exact variable. Whichever vendor ran - including the unflagged default -
is recorded as `vendor: anthropic (default, no flag given)` or `vendor:
<name>` in both the printed JSON report and the posted `examiner` status, so
the choice is never silent.

`--changed-files` is Fabrica's names-only list of every worker-changed file:

```json
{
  "baseCommit": "<task-starting-commit>",
  "files": [{"path": "backend/capture_test.go", "change": "modified"}]
}
```

It has no file contents or diff. `--base-tests` supplies only the starting-commit
content of changed test files, never the worker's version:

```json
{
  "baseCommit": "<task-starting-commit>",
  "tests": [{"path": "backend/capture_test.go", "content": "...pre-task test..."}]
}
```

A changed test name is a signal. The examiner derives the old test's protected
scenario and drives it in the app. A new test with no pre-task version triggers
nothing. Fabrica issue #101 will produce both artifacts; until then, callers
supply them explicitly. Missing or malformed lists refuse rather than reading as
no changed tests.

The sealed container receives copies of only these inputs and a writable
temporary output directory. It has no repository mount or implementation diff.
The locally built runtime forces the trusted agent entrypoint. The model has no
shell or arbitrary-network tool: it can call only the supplied app URL, and a
redirect cannot leave that origin. A local Docker daemon is required because
bind mounts resolve on the daemon host; remote Docker endpoints are refused.
The container runs under whatever user namespace the daemon puts it in rather
than opting out of one, so a host configured with Docker `userns-remap` keeps
that isolation while being examined. Because such a daemon runs the container
as a different, unprivileged host user, inspector asks the container which host
identity it actually got and grants exactly that one uid the least access it
needs to the staged inputs and the verdict it fills in - traversal of the two
directories, read on each input, write on the verdict file, and nothing to any
other account on the host. On a daemon that remaps nothing the container
reports the identity inspector already runs as and nothing is granted. A host
where even that single grant cannot be arranged refuses rather than loosening
the staged modes.

`userns-remap` nevertheless remains **declared unsupported** until that path has
been recorded working on a live remapping daemon, which has not happened yet -
the bridge above is built and unit-tested, and
`TestExaminerStagedAccessUnderUsernsRemap` verifies it end to end but skips
where no such daemon is reachable. See
[#32](https://github.com/inactdev/inspector/issues/32).

The model proposes capabilities, attempts, and assessments. Inspector records
which capability each app request drove and what response was delivered, then
derives every per-capability result from that record. A capability with no
successful observation is `could_not_be_tested` and cannot be marked confirmed.
The JSON verdict also carries `examinationIncomplete`, so an untested capability
never hides a confirmed bug and a bug never hides an incomplete examination.
Confirmed missing behavior and failed pre-task-test scenarios include proposed
permanent regression lines; Inspector never edits the project.

The examiner posts its own status under the stable context `examiner`:
`success` only when every capability is confirmed and the examination completed,
`failure` when it found a bug, and `error` for an otherwise incomplete or
explicitly refused examination. When both a bug and incompleteness are present it
posts `failure` and names both facts. A complete success exits `0`, a found bug
exits `1`, and an incomplete or refused examination exits `2`. A missing
`--owner`, `--repo`, or `--commit`, or a malformed `--commit`, exits `2` before
posting because there is no valid status destination; a status-post failure also
exits `2`.

Both blocking states are distinct on the pull request page. Branch protection
must require `examiner` alongside `gate`; the inspector-gate setup below owns the
repository-setting instructions. An owner can use GitHub's visible admin
override when that is the right human decision.

This repository temporarily protects `*_test.go` through `.inspector.json` under
[fabrica#96](https://github.com/inactdev/fabrica/issues/96). A pull request that
adds or changes required Go tests therefore remains intentionally red at `gate`
until the owner reviews it and uses that override. A delivery pipeline that
publishes the branch without Inspector's stage, stamp, then publish sequence also
leaves that commit without the required `inspector` status. Neither condition is
a CI defect that a follow-up source change can make green. Do not remove or rename
tests, weaken the protected-path policy, or create a status merely to make such a
change green. What the reviewer's `--test-paths` judgment adds is the reason for
that override, not a way around it - run it with `--test-paths '*_test.go'` on
such a change and the verdict says whether the exam survived.

## Always-true list

`inspector always` is check 3: the project's **regression list**, driven end to
end. The list is the set of things a person would run by hand to convince
themselves the product still works - console commands, driver scripts, HTTP
calls - and **all of them must still pass**
([#24](https://github.com/inactdev/inspector/issues/24)).

It lives in the judged repository, at `.inspector-always.json` by default,
under that project's `protectedPaths`. That is what makes it hard to weaken
quietly, and it means the list travels with the code rather than living in a
tool's configuration. It is deliberately not like `.inspector.json` and the
gate files, which are the rules of the game and stay human by design: this list
is accumulated knowledge about the product and has to be able to grow. A change
to it is **not forbidden** - it has to be *visible*, and whether it is
legitimate is decided by whether the request justifies it, the same rule as a
test change. The protected-path gate is what makes it visible.

It is also the examiner's `--always-true` input. One name, one file, one check:
the invariants a project states and the always-true list the examiner hunts
around are the same artifact.

### What a line looks like

Each line carries both halves - the plain-language promise a person reads, and
the thing that would falsify it:

```json
{
  "version": 1,
  "app": {
    "image": "inkwell-backend:local",
    "args": ["--addr", "0.0.0.0:8080", "--storage-dir", "/fixture/storage"],
    "port": 8080,
    "fixture": { "dir": "/fixture", "files": { "storage/.keep": "" } },
    "ready": { "path": "/inklings", "status": 200, "timeout": "60s" }
  },
  "runner": { "image": "curlimages/curl:8.5.0" },
  "promises": [
    {
      "id": "inkling-never-lost",
      "promise": "An inkling is never lost once captured.",
      "confirmed": true,
      "source": "proposed by a confirmed check-2 finding, 2026-10-24",
      "falsify": {
        "http": [
          { "method": "POST", "path": "/inklings",
            "headers": { "Content-Type": "application/json" },
            "body": "{\"id\":\"...\"}",
            "expect": { "status": 201 } },
          { "method": "GET", "path": "/inklings",
            "expect": { "status": 200, "bodyContains": ["..."],
                        "bodyNotContains": ["the superseded text"] } }
        ]
      }
    }
  ]
}
```

`falsify` carries exactly one of the three shapes:

- `http` - a sequence of requests Inspector performs itself. Every step must
  answer its `expect` (a status, and optionally substrings its body must or
  must not contain); the first that does not breaks the promise.
- `command` - argv run in a single-use container from `runner.image`, with the
  app at `$APP_URL`. Exit 0 means the promise survived.
- `script` - a driver script, named by its **absolute path inside
  `runner.image`**. It cannot be a path in the project: check 3 mounts no
  repository, so there is no project tree to name one in.

Two rules about what is *not* there:

- **A line with no `falsify` is reported as not runnable, and never as
  passing.** That is how a promise nobody can disprove yet stays visible
  instead of quietly counting as a pass. Give it a `note` saying why.
- **A `falsify` that is declared but empty is refused when the list is read.**
  So is an unknown or duplicated field anywhere in the file - a misspelled
  expectation that decoded to nothing would leave a promise passing against an
  assertion nobody made.

`confirmed` records whether a person has accepted the line. A proposal sits
there with `false` until someone does; it is still driven, still counted, and
the run says how many lines are still proposals. It deliberately does not
change the verdict - a broken promise nobody accepted yet is still a broken
promise.

### Running it

```
GITHUB_TOKEN=... inspector always \
  --list .inspector-always.json \
  --commit <40-character-commit-sha> \
  --owner inactdev \
  --repo inkwell
```

Check 3 **brings the app up itself**, from the image the list names, with the
fixture data the list declares, waits for the declared readiness response, and
removes both afterwards. A promise is only evidence about this commit if the
thing it was driven against was brought up from a declared image with known
data for that one run; a long-lived instance's accumulated state can make a
promise pass vacuously. The image must already be present locally - like the
examiner's runtime, check 3 never builds or pulls while it holds app access.
The app container is read-only apart from its fixture mount and its own `/tmp`,
drops every capability, and is memory-, CPU-, and process-bounded.

It is **read-only by construction, not by instruction**: it mounts no
repository at all, the same boundary the examiner has, so there is no handle on
the commit for it to edit and nothing to prove about that afterwards.

HTTP falsifiers run against the ephemeral loopback port Docker published for
the app. Command and script falsifiers run in their own container joined with
`--network container:<app>`, sharing the app container's network stack, which
is why `$APP_URL` names `127.0.0.1` and the port the app itself listens on.

### Its verdict

One aggregate, never fail-fast: every line is attempted even after one breaks,
so the whole list's state arrives in a single status under the stable context
`inspector-always`.

- `success` - every promise ran and every one of them held. Exits `0`.
- `failure` - at least one promise broke, and the status names all of them.
  Exits `1`. A broken promise outranks incompleteness and does not hide it:
  both facts stay in the result.
- `error` - nothing broke, but the list was not fully checked: the app would
  not start, or a line declares no falsifier. It says which. Exits `2`, as does
  a refusal that never got as far as the list.

`inspector run` does not invoke check 3, and the gate script does not read its
status: it is its own command and its own judgment, the same as
`inspector examine`. For that reason `inspector-always` is deliberately not a
required status check yet - a required status nobody posted never passes, which
would block every pull request. The inspector-gate section below says when to
add it.

Two parts of issue #24 are deliberately **follow-ups**, not here: the pipeline
that turns a confirmed check-2 finding into a proposed line in this list, and
the judgment of a change to the list against the request it was made for.

Inspector's own [`.inspector-always.json`](.inspector-always.json) is seeded
with promises inferred from its own tests, every one of them a proposal nobody
has confirmed and none of them carrying a falsifier yet - so `inspector always`
run against this repository reports `error` and names each line as not
runnable. Inspector is a command-line tool with no HTTP app to bring up, and
the no-repository-mount boundary is exactly what keeps those lines unrunnable
for now. `examples/inkwell/inkwell-always.json` is the worked example of a list
with real falsifiers in it.

## inspector-gate

`.github/workflows/inspector-gate.yml` is the cloud half (SPEC.md sections 2, 4,
5, 7, 8). It asks exactly two questions about a pull request, and answers both
with no AI and no checkout: does the head commit carry a green `inspector` commit
status (no status at all is red, same as a failing one), and did the pull request
touch a protected path (any touch is red - there is no declared-changes path that
passes, with the one narrow `testPaths` exception described below; see
`.github/scripts/inspector-gate.sh` for the full reasoning inline).

Adopting it means copying **both** files. The workflow never checks the
repository out, so it reads `.github/scripts/inspector-gate.sh` from the base
branch through the GitHub API and runs that; with the workflow alone in place,
every run fails closed saying it could not load its own definition.

Protected paths are `.inspector.json`, `Dockerfile.inspector`, and everything
under `.github/` always, plus whatever a project lists under `protectedPaths`
in its `.inspector.json`:

```json
{
  "check": "check.sh",
  "protectedPaths": ["check.sh", "deploy/*", "*.tf"]
}
```

One glob pattern per entry. A `*` matches any characters, `/` included, so
`deploy/*` already covers everything at any depth under `deploy/`. There is no
separate recursive `**` wildcard - it is just two `*` in a row, so a leading
`**/` still requires a literal `/` and `**/*.tf` will *not* match a `main.tf`
at the repository root. Write `*.tf` when you mean every `.tf` file.

`Dockerfile.inspector` is in that floor because the check environment used to
be named by `.inspector.json`'s `"image"` field, which the floor already
covered: swapping the box a check runs in is another way to make a failing
check stop failing, so moving that declaration into its own file must not
narrow what the gate protects.

**Your own check command is not protected automatically - list it under
`protectedPaths` yourself.** This was tried the other way: deriving a pattern
from `check`'s own value, on the reasoning that it is exactly what a worker
under pressure edits to make a failing check stop failing. It doesn't hold up -
telling a path from a command line by looking at the string alone doesn't work.
`"script/check"` is a path, `"pytest"` is not, and `"check.sh"` could be either;
every heuristic that tries to split them keeps producing wrong answers on one
side or the other. So the gate no longer guesses: a bare-path check command and
a multi-word one are both left for the project to declare, explicitly, the same
way. Making that hard to forget belongs to the installer (inspector#5), which
knows the concrete value at install time and can write it where a human sees
and confirms it - guessing at runtime is the wrong layer for that guarantee.

That list is read from the *base* branch's current tip, never the pull request's
copy - the same place GitHub loads the workflow file itself from. Adding an
entry to `protectedPaths` therefore applies immediately to pull requests that
are already open, which also means a pull request's verdict can change without
the pull request changing. That is the intended direction for a protection list.
If the list cannot be read with certainty - a rate limit, a server error, a file
too large for the API to inline, invalid JSON, a top level that is not a JSON
object, or a `protectedPaths` (or `testPaths`) that is not a list of strings -
the gate fails closed and says which of those it hit, rather than quietly
judging against a shorter list. Only a genuinely absent `.inspector.json`
(HTTP 404) is a safe absence; that project still gets the always-protected
floor.

**One exception clears a protected path: a judged test file.** When the same
`.inspector.json` declares `testPaths`, a protected path matching one of those
patterns passes if the head commit carries a green
[`inspector-tests` status](#the-inspector-tests-status) - inspector's record
that every changed test in that commit still proves what the request asked for.
Anything else keeps it red: a `failure`, a stale state, or no such status at
all. The gate itself stays AI-free either way; it compares a status string, the
same as for question 1.

Nothing else clears a protected path, and the always-protected floor
(`.github/**` and `.inspector.json`) is never clearable this way, even if a
project lists those patterns under `testPaths` too - a project must not be able
to hand the gate's own definition to a model's judgment. A `testPaths` entry
only ever downgrades a path the project's own `protectedPaths` made red in the
first place; a test file nobody protected was never blocked and needs nothing
to pass.

**The first gate run does not go stale-red.** inspector stages a green commit,
posts its status, and only then updates the pull request branch. The gate
therefore sees the status on the push that wakes it. An `on: status` trigger is
not a replacement for that ordering: GitHub attaches a status-triggered run to
the default branch's last commit rather than to the commit the status was
posted to, so it cannot flip the pull request's own check. The gate only reads
statuses and needs `statuses: read`, not `statuses: write`.

**Copying this workflow into a repo does not, by itself, block a merge.** GitHub
only enforces a check once it is a *required* status check:

> Settings -> Branches -> add (or edit) a branch protection rule for the default
> branch (`main` in this repository) -> enable "Require status checks to pass
> before merging" -> add **gate** (the job's name) and **examiner** (the separate
> commit status) to the list.

The existing `gate` job continues to require the separate `inspector` status.
`inspector-tests` is deliberately *not* on that list: it is read by the `gate`
job itself, which is already required, so adding it separately would block
every pull request that changes no test file at all - inspector posts no such
status then, and a required status that is never posted never passes. This is a
repository setting the workflow cannot turn on for itself - there is no API call
or workflow step that does it from here.

`inspector-always` is not on that list yet either, and for the second half of
the same reason: nothing invokes check 3 as part of `inspector run`, so the
status is absent on any commit where a caller did not run `inspector always`
itself. Make it required once the delivery pipeline runs check 3 on every
commit it hands over - not before, or every pull request blocks on a status
nobody posted.

### Keeping a consumer's copy of the gate in step

Adopting the gate means copying two files into your repository, and a copy
goes stale the moment either of them changes here. `.github/workflows/gate-sync.yml`
is what closes that gap for repositories that want it: a push to `main` that
changes `.github/workflows/inspector-gate.yml` or
`.github/scripts/inspector-gate.sh` opens - or updates - **one** pull request
on **one** fixed branch, `inspector-gate-sync`, on every repository listed in
`.github/gate-consumers.txt`, carrying both files verbatim and naming the
inspector commit they came from. `.github/scripts/gate-sync.sh` holds the
decision and does the work; `gate-sync_test.sh` tests it and checks the
workflow's own facts against it.

It opens pull requests and never merges one. A consumer reviews the change to
its own gate like any other, which is the same rule the gate itself applies to
protected files.

Running it on another repository needs two things that are not in this one:

- **A `GATE_SYNC_TOKEN` repository secret**, scoped to exactly the consumer
  repositories, with **Contents** and **Pull requests** write access. The
  default `GITHUB_TOKEN` cannot reach another repository at all, so there is
  no fallback and the sync fails closed, by name, when the secret is missing.
- **Each consumer listed in `.github/gate-consumers.txt`**, one `owner/repo`
  per line. That file sits inside the always-protected `.github/**` floor, so
  pointing a write-scoped token at a new repository needs the owner's eyes,
  like any other gate change.

Everything about it fails closed. A list that does not parse - a malformed
line, a repository named twice, this repository naming itself - syncs nothing
at all, rather than acting on the part of it that did parse. A consumer whose
current state cannot be read with certainty is never written to. And when one
consumer fails while others succeed, the run still attempts every one of them
and then fails at the end naming each failure and its reason: one repository's
revoked access must not leave the rest carrying a stale gate. Re-running it
after fixing the cause does nothing to the consumers that are already up to
date.

## Status

The check (issue #1) is built: locally, `inspector` runs a project's own check
command against HEAD, inside a container (issue #13), and reports green or
red, refusing loudly when no check command is configured, when the project
commits no `Dockerfile.inspector` to build that container from, or when no
usable container runtime is found. That container is memory-, CPU-, and
process-bounded and runs as the inviting host user rather than root (issue
#43), and the commit under check is mounted read-only inside it with a
throwaway writable workspace in its place, so a check command cannot modify
the code it is being judged on (issue #27). Issue #47 closed the last part of
that: the image is built from the project's own committed Dockerfile at check
time, a project without one is refused by the file's name, and the container
now runs with no network and no opt-in to ask for one - the retired `"image"`
and `"network"` config keys are refused rather than ignored.
Inspector-owned publication (issue #18) is
also built: it stages a green commit, posts its status, then moves an explicitly
named non-default branch; a red result remains local by v1 policy. That branch
update is fast-forward only unless the caller authorizes replacing its history
with `--expected-tip`, which is what lets a rebased branch be published. The HTTP
examiner (issue #7, above) and the gate workflow (issue #4) are also built. The
gate reads the project-check status under the context `inspector`. The code
reviewer (issue #34, above) is also built: it shares the `internal/llm` vendor
seam with the examiner, and with `--test-paths` it judges the change's test
files for a weakened exam. Issue #44 is built on top of that judgment: a
project declaring `testPaths` gets it run by `inspector run`, recorded as the
`inspector-tests` status, and read by the gate as the one thing that clears a
changed test file - a stopgap for the blanket test protection above, for as
long as that protection lasts. Issue #55 keeps that judgment auditable: every
per-file judgment and reason, pass included, is printed and recorded in the run
report's `testReview`, while the status itself is unchanged. The documentation
judgment (issue #16, above) is built as well: `inspector document` asks both
whether the change is described and - the question that gets missed - whether
the change made something already written untrue, re-reading the project's own
tracked text so it finds the stale claim in files the change never touched. It
reports and posts no status. The guidebook generator (issue #22, above) is
built: `inspector guidebook` extracts a project's deterministic facts itself,
has a model write the examiner's operating instructions over them, renders one
fixed-shape `INSPECTOR-GUIDEBOOK.md` at the project root without ever
overwriting a confirmed one, and leaves it unconfirmed until a person signs it -
which the examiner now reads, recording every capability driven from an
unsigned guidebook as could-not-be-tested. Check 3, the always-true list
(issue #24, above), is built: the project's regression list of promises lives
in its own repository under `protectedPaths`, each line carrying the
plain-language promise and the command, script, or HTTP call that would
falsify it, and `inspector always` brings the app up itself, drives every
line, and posts one aggregate `inspector-always` status - failure naming every
broken promise, error when the list could not be fully checked. The two
remaining parts of that issue are follow-ups: turning a confirmed check-2
finding into a proposed line, and judging a change to the list against the
request. The gate-file sync (above) is built too: a
change to either gate file on `main` opens or updates one pull request per
listed consumer, carrying both files and naming the commit. Still to come: the
installer - see
[SPEC.md](SPEC.md) and the repo's issues.
