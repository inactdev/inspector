<img width="1280" height="720" alt="inspector" src="https://github.com/user-attachments/assets/a8685b63-7b08-4102-9d09-bceb811ffc68" />

# inspector

The shipping gate. It checks that finished work is actually finished, repairs what
it can, and blocks the merge when it can't.

Read [SPEC.md](SPEC.md) first - it explains why this exists, which is the part that
matters. The short version: a tool that reports on its own work can tell you what it
did, what it assumed, and what it skipped, but it can never establish that its own
report is true. inspector is the thing outside that checks the claim, using the
project's own checks rather than anyone's description of them.

It has two halves:

- **inspector** runs on your machine. It runs the project's checks, repairs what it
  can, re-verifies, records a green result against the exact commit it checked, and
  only then publishes the explicitly named pull request branch.
- **inspector-gate** runs on GitHub. Small and dumb on purpose: does this commit
  carry a green inspector result, and were any protected files touched.

It runs when something claims to be finished, never on push - a fixer let loose on
half-written work repairs code that was mid-change.

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
repo (and nothing else on the host) at `/workspace`, its own working
directory. If the image is missing or unusable, inspector refuses the same way
it refuses a missing check command, rather than quietly doing nothing.

The container gets no network access by default. A check command that needs
it - installing dependencies is the common case - has to opt in:

```json
{
  "check": "npm install && npm test && npm run lint",
  "image": "node:20",
  "network": true
}
```

That default is deliberate, not incidental: the same container that keeps a
hostile or broken check command from reaching the rest of your machine is also
the thing standing between it and the network, which is how it would exfiltrate
anything it found. Turning `network` on trusts the check command with outbound
access; leave it off for anything you haven't read.

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

A third field, `protectedPaths`, is read only by inspector-gate, never by
inspector itself - see the "inspector-gate" section below.

The order is commit, then inspect. inspector refuses to run against an
uncommitted working tree - it can only be honest about a commit that actually
matches what's on disk. For a green commit C, inspector owns the remaining
publication steps, in this exact order:

1. It runs the check locally on C.
2. It transfers C to a temporary non-branch staging ref on `origin`.
3. It records C's green commit status.
4. It updates the explicitly named pull request branch on `origin`.

Name that destination on every invocation:

```
inspector --branch my-feature
```

`--branch` is required. Inspector never derives publication authority from the
checkout, HEAD, tracking configuration, or any other ambient state, and it
refuses the remote default branch even when explicitly named. This is a captain
decision that may be overruled: ambient destination authority conflicts with
the architecture's explicit handoff, and a freshly pulled checkout is normally
on the default branch, where an inferred push would bypass the pull request.

The staging ref exists only so GitHub has C when it receives the status. It is
removed after the named branch moves. Inspector confirms C is still HEAD before
the staging push. The pull request branch never points at C until its status
already exists, so inspector-gate's first run sees the result instead of a
stale missing-status failure.

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

Exit codes reserve `0`, `1`, and `2` for verdicts only:

- `0` green
- `1` red
- `2` refused - inspector tried to reach a verdict and couldn't: no check
  command or image configured, no usable container runtime, a dirty working
  tree, a platform where publication cannot safely terminate a timed-out git
  process tree, a check command that ran past its timeout or was killed by a
  signal before it could finish on its own (the OOM killer, an external kill,
  inspector's own deadline - it never judged the code, so its exit status is
  not a verdict either way), an infrastructure failure, or a local green that
  inspector could not publish completely (see "Recording the result" below) -
  an incomplete result proves nothing to the gate, so it isn't a verdict
  either. An ordinary refusal makes no remote change; an incomplete green
  publication may leave the staging ref or status described below. Never treat
  `2` as red.

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
never changes an answer inspector already has. When the check command reached a
real green or red and only the save failed, inspector prints a loud warning to
stderr naming where the save failed, and still exits `0` or `1` with that
verdict - it does not become a refusal.

### Recording the result

A green result gets posted to GitHub as a **commit status** on the exact
commit inspected, under the status context `inspector` - a small record
attached to that commit, not a comment or a checklist, because text can be
typed by anyone and a status cannot. It carries pass and a short description
pointing at `.inspector/latest.json`; the report holds the detail, the status
just says whether to trust it.

This needs a token: set `GITHUB_TOKEN` to one with commit-status write access
on the repo, and `origin`'s push URL must point at the same GitHub repository
that receives the status. Pushes are non-interactive and time out rather than
waiting forever for credentials or a stalled transport; a timeout kills git and
its helper processes together. On platforms where Inspector cannot guarantee
that complete process-tree termination, it refuses before running checks or
starting any publication step. Posting is not optional - a missing token, an
unresolvable remote, a failed staging push, or
GitHub refusing the request all fail loudly and exit `2`, even when the local
check passed. A real local green that inspector could not publish in full is
worth nothing to a reader who can only see GitHub, so it must never look like
success.

Once publication starts, inspector does not try to erase partial remote state.
A failed network operation can mean either that GitHub rejected the operation or
that GitHub completed it but the client lost the response. Inspector therefore
reports uncertain remote state honestly: a failed staging push may have created
the staging ref; a failed status post may have recorded the status; and a failed
final push may have moved the named branch. It does state which later operations
were never attempted. These incomplete publications are distinct from an
ordinary refusal and exit `2`. This is the captain's deliberate policy and may
be overruled: compensation cannot make a partially published sequence atomic.

What a reader should conclude:

- **Green status** - inspector ran this exact commit's checks and they passed.
- **Red status** - a manually recorded failure. v1 inspector does not publish
  red work or post this status itself.
- **No status at all** - this commit has not been approved, or inspector
  reached a verdict locally but could not record its green status. Read the
  same as red. A status from an earlier commit does not carry forward.

The token is not an identity boundary - see SPEC.md section 7 for the honest
limit on what a green status does and doesn't prove.

## inspector-gate

`.github/workflows/inspector-gate.yml` is the cloud half (SPEC.md sections 2, 4,
5, 7, 8). It asks exactly two questions about a pull request, and answers both
with no AI and no checkout: does the head commit carry a green `inspector` commit
status (no status at all is red, same as a failing one), and did the pull request
touch a protected path (any touch is red - there is no declared-changes path that
passes; see `.github/scripts/inspector-gate.sh` for the full reasoning inline).

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
object, or a `protectedPaths` that is not a list of strings - the gate fails
closed and says which of those it hit, rather than quietly judging against a
shorter list. Only a genuinely absent `.inspector.json` (HTTP 404) is a safe
absence; that project still gets the always-protected floor.

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
> branch -> enable "Require status checks to pass before merging" -> add **gate**
> (the job's name) to the list.

That is a one-click repository setting this workflow cannot turn on for itself -
there is no API call or workflow step that does it from here.

## Status

The check (issue #1) is built: locally, `inspector` runs a project's own check
command against HEAD, inside a container (issue #13), and reports green or
red, refusing loudly when no check command or image is configured, or no
usable container runtime is found. Inspector-owned publication (issue #18) is
also built: it stages a green commit, posts its status, then moves an explicitly
named non-default branch; a red result remains local by v1 policy. The gate
workflow (issue #4, above) reads that status under the context `inspector`.
Still to come: the fixer and
the installer - see [SPEC.md](SPEC.md) and the repo's issues.
