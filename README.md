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
`.inspector.json` declares `"image": "cimg/go:1.22"` rather than `golang:1.22`:
it carries all of them together, and the check container has no network to
install anything with.

Also requires [Docker](https://docs.docker.com/get-docker/) - the check
command runs inside a container, not on your machine directly. Without a
usable Docker (installed and its daemon reachable), inspector refuses rather
than falling back to running the check unsandboxed; see below.

## Use

In the repo you want inspected, add `.inspector.json` at the root, naming that
project's own check command and the container image it runs in - inspector
never guesses at either:

```json
{
  "check": "npm test && npm run lint",
  "image": "node:20"
}
```

`image` has to have the project's own toolchain in it, the same way `check`
has to be the project's own command - inspector ships no images and builds
none for you. The check command runs with that image, bind-mounted to the
repo at `/workspace`, its own working directory. If the image is missing or
unusable, inspector refuses the same way it refuses a missing check command,
rather than quietly doing nothing.

One other thing is reachable from inside that container, and only when the
repo needs it: its own git directory. An ordinary clone keeps that inside the
repo, so nothing extra is mounted. A **linked worktree** (`git worktree add`)
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

The container gets no network access by default. A check command that needs
it - installing dependencies is the common case - has to opt in:

```json
{
  "check": "npm ci && npm test && npm run lint",
  "image": "node:20",
  "network": true
}
```

That default is deliberate, not incidental: the same container that keeps a
hostile or broken check command from reaching the rest of your machine is also
the thing standing between it and the network, which is how it would exfiltrate
anything it found. `network` is honored for now, but it is a deliberately
temporary opt-in, not a settled design: the eventual intent is that nothing
inside the check container ever needs the network at all, once a project
commits its own image with its dependencies already baked in rather than
installing them at check time - tracked as issue #47, not yet built (see
"Status" below). Every run that uses `network: true` logs that on stderr, in
the report alongside the check's own output, as a standing reminder that it is
going away. Turning it on trusts the check command with outbound access in the
meantime; leave it off for anything you haven't read.

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
  "image": "golang:1.22",
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
  "image": "node:20",
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
6. It updates the explicitly named pull request branch on `origin`.
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

This recheck compares only the post-check snapshot. In v1 the source mount is
writable, so a check can modify a tracked file, pass against those modified
bytes, and restore the file before exiting without being detected. A resulting
green can therefore describe code that was never the commit stamped. Issue
[#27](https://github.com/inactdev/inspector/issues/27), "Source is read-only
while the check runs," is where that gap is closed.

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
    container runtime, a dirty working tree, a timeout, or infrastructure
    failure, and green-publication preflight failures such as an unsupported
    platform, missing token, invalid remote, or default-branch target. A refusal
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
output. These are notes for a
human, never authority. On its first run inspector
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
the staging ref, and a failed final push may have moved the named branch. An
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
  "image": "cimg/go:1.22",
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

## Examiner

`inspector examine` is the independent outcome judge. It derives scenarios from
the request, operates an already-running HTTP app, and never reads the judged
application's implementation or worker-written test content.

The first version drives HTTP backends. It does not drive native iOS screens, so
an Inkwell examination covers its backend only. The hand-written first
[Inkwell feature map](examples/inkwell/EXAMINER_GUIDEBOOK.md) and
[always-true list](examples/inkwell/INKWELL_ALWAYS_TRUE.md) are worked examples.
Issue #22 will generate confirmed feature maps for other projects, and #24 owns
the always-true list producer.

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
Docker `userns-remap` is unsupported for now because private staged inputs
cannot safely cross that mapping - see [#32](https://github.com/inactdev/inspector/issues/32).

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

Protected paths are `.inspector.json` and everything under `.github/` always,
plus whatever a project lists under `protectedPaths` in its `.inspector.json`:

```json
{
  "check": "check.sh",
  "image": "node:20",
  "protectedPaths": ["check.sh", "deploy/*", "*.tf"]
}
```

One glob pattern per entry. A `*` matches any characters, `/` included, so
`deploy/*` already covers everything at any depth under `deploy/`. There is no
separate recursive `**` wildcard - it is just two `*` in a row, so a leading
`**/` still requires a literal `/` and `**/*.tf` will *not* match a `main.tf`
at the repository root. Write `*.tf` when you mean every `.tf` file.

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
red, refusing loudly when no check command or image is configured, or no
usable container runtime is found. That container is memory-, CPU-, and
process-bounded and runs as the inviting host user rather than root (issue
#43); it has no network access by default, with a temporary `network: true`
opt-in still honored until projects commit their own image with dependencies
baked in - issue #47, not yet built. Inspector-owned publication (issue #18) is
also built: it stages a green commit, posts its status, then moves an explicitly
named non-default branch; a red result remains local by v1 policy. The HTTP
examiner (issue #7, above) and the gate workflow (issue #4) are also built. The
gate reads the project-check status under the context `inspector`. The code
reviewer (issue #34, above) is also built: it shares the `internal/llm` vendor
seam with the examiner, and with `--test-paths` it judges the change's test
files for a weakened exam. Issue #44 is built on top of that judgment: a
project declaring `testPaths` gets it run by `inspector run`, recorded as the
`inspector-tests` status, and read by the gate as the one thing that clears a
changed test file - a stopgap for the blanket test protection above, for as
long as that protection lasts. The gate-file sync (above) is built too: a
change to either gate file on `main` opens or updates one pull request per
listed consumer, carrying both files and naming the commit. Still to come: the
installer - see
[SPEC.md](SPEC.md) and the repo's issues.
