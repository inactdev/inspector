# Project agent memory

This file is the project's committed home for project-intrinsic agent knowledge: build, test, release, architecture, and sharp-edge notes that should travel with the code.

- Read SPEC.md before touching anything - it is the design of record and short.
- This repo's own check command is `script/check` (gofmt, vet, build, test), wired up via `.inspector.json` - inspector dogfoods itself.
- Two similarly-named paths, different jobs: `.inspector.json` (tracked, project config - the check command, and later the protected-file list) vs `.inspector/` (gitignored, one JSON report per local run, notes only, never authority).
- `internal/inspector.Run` refuses on a dirty working tree, because a result is only honest when it's bound to a commit that actually matches what's on disk. It excludes `.inspector/` itself from that dirty check via a git pathspec (`repo.go`, `WorkingTreeStatus`) - without that exclusion, a repo that never gitignores `.inspector/` would go permanently dirty, and therefore permanently refused, after its first run. Don't drop that exclusion.
- Exit codes are a deliberate three-way split, not pass/fail: `0` green, `1` red, `2` refused (no config, dirty tree, or a `--commit` mismatch). `2` means no verdict was reached at all - callers (the eventual gate, issue #3/#4) must not treat it as red.
- This is issue #1 of the roadmap in `gh issue list` (or `gh-axi issue list` under firstmate) - the fixer (#2), posting a commit status (#3), the gate workflow (#4), and the installer (#5) are deliberately out of scope here and still open.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
