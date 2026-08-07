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

## Status

Specification only. Nothing is built yet.
