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
green or red. Exit codes: `0` green, `1` red, `2` refused (no check configured, a
dirty working tree, or a `--commit` mismatch) - never treat `2` as red, it means
inspector didn't reach a verdict at all.

Flags:

- `--repo <path>` - inspect a repo other than the current directory
- `--commit <sha>` - assert HEAD equals this commit; refuse rather than silently
  inspecting the wrong one

Anything after the flags is a free-text claim, recorded for context and not acted
on:

```
inspector implemented the login flow
```

Each run writes a small JSON report to `.inspector/` (gitignored - notes for a
human, never authority) so a red result is actionable without re-running: the
command that ran, its exit code, and its full output.

## Status

The check (issue #1) is built: locally, `inspector` runs a project's own check
command against HEAD and reports green or red, refusing loudly when no check
command is configured. Still to come: the fixer, posting the result as a GitHub
commit status, the gate workflow, and the installer - see [SPEC.md](SPEC.md) and
the repo's issues.
