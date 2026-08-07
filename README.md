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
  can, re-verifies, and records the result against the exact commit it checked.
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
compiler.

## Use

In the repo you want inspected, add `.inspector.json` at the root, naming that
project's own check command - inspector never guesses at one:

```json
{
  "check": "npm test && npm run lint"
}
```

Then, with your work committed (inspector refuses to run against an uncommitted
working tree - it can only be honest about a commit that actually matches what's
on disk):

```
inspector
```

This runs the configured check command against the repo's current HEAD and prints
green or red.

Exit codes reserve `0`, `1`, and `2` for verdicts only:

- `0` green
- `1` red
- `2` refused - inspector tried to reach a verdict and couldn't: no check
  command configured, a dirty working tree, a `--commit` that doesn't resolve
  to HEAD, a check command killed by a signal before it could finish on its own
  (the OOM killer, an external kill - it never judged the code, so its exit
  status is not a verdict either way), or an infrastructure failure. Never
  treat `2` as red.
- `64` usage - not a verdict attempt at all: `--help`, an unrecognized flag, or
  bad usage. Distinct from `0`/`1`/`2` so a caller can never mistake a help
  request for a result; `64` follows the BSD
  [sysexits.h](https://man.freebsd.org/cgi/man.cgi?query=sysexits) convention
  for a command-line usage error.

Flags:

- `--repo <path>` - inspect a repo other than the current directory
- `--commit <ref>` - assert HEAD resolves to this commit; accepts anything git
  itself would resolve unambiguously (a full SHA, a short prefix, a branch, a
  tag), resolved through `git rev-parse` the same way git would resolve it.
  Refuses rather than silently inspecting the wrong commit both on a genuine
  mismatch and on a `ref` that doesn't resolve to exactly one commit (an
  ambiguous short prefix, or one that doesn't exist)

Anything after the flags is a free-text claim, recorded for context and not acted
on:

```
inspector implemented the login flow
```

Each run writes a small JSON report to `.inspector/runs/`, and copies it to
`.inspector/latest.json`, so a red result is actionable without re-running: the
command that ran, its exit code, and its full output. These are notes for a
human, never authority. On its first run inspector
writes `.inspector/.gitignore` containing `*`, so reports stay out of git without
you editing anything; an existing `.inspector/.gitignore` is left alone.

Because the report is notes and not authority, a report that can't be saved
never changes an answer inspector already has. When the check command reached a
real green or red and only the save failed, inspector prints a loud warning to
stderr naming where the save failed, and still exits `0` or `1` with that
verdict - it does not become a refusal.

## Status

The check (issue #1) is built: locally, `inspector` runs a project's own check
command against HEAD and reports green or red, refusing loudly when no check
command is configured. Still to come: the fixer, posting the result as a GitHub
commit status, the gate workflow, and the installer - see [SPEC.md](SPEC.md) and
the repo's issues.
