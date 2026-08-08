# Project agent memory

This file is the project's committed home for project-intrinsic agent knowledge: build, test, release, architecture, and sharp-edge notes that should travel with the code.

- Read SPEC.md before touching anything - it is the design of record and short.
- This repo's own check command is `script/check` (gofmt, vet, build, test), wired up via `.inspector.json` - inspector dogfoods itself. Tests run under `-race`, which needs cgo and a C compiler locally (CI's ubuntu-latest already has one) and costs about a second of suite time; keep it, the code captures subprocess output concurrently. `-race` is unsupported on some platforms (32-bit x86, linux/arm, freebsd/arm) - use `CGO_ENABLED=0 go test ./...` there instead of `script/check`.
- Two similarly-named paths, different jobs: `.inspector.json` (tracked, project config - the check command, and later the protected-file list) vs `.inspector/` (gitignored, one JSON report per local run, notes only, never authority).
- `internal/inspector.Run` refuses on a dirty working tree, because a result is only honest when it's bound to a commit that actually matches what's on disk. It excludes `.inspector/` itself from that dirty check via a git pathspec (`repo.go`, `WorkingTreeStatus`) - without that exclusion, a repo that never gitignores `.inspector/` would go permanently dirty, and therefore permanently refused, after its first run. Don't drop that exclusion.
- Exit codes are a deliberate split, not pass/fail: `0`/`1`/`2` are reserved for verdicts, and anything that is not a verdict attempt at all gets `64`. The list itself is owned by README's Use section and the const block in `cmd/inspector/main.go` - read it there, don't restate it. The invariant callers depend on (the eventual gate, issue #3/#4): never treat `2` as red, and never confuse `64` with a verdict.
- This is issue #1 of the roadmap in `gh issue list` (or `gh-axi issue list` under firstmate) - the fixer (#2), the gate workflow (#4), and the installer (#5) are deliberately out of scope here and still open.
- `RunCheck` (`check.go`) puts the check command's process in its own process group (`Setpgid`) and kills that whole group, not just the direct `sh` child, when the timeout fires. `sh -c "a && b"` forks a child per command, so killing only `sh` leaves a hung compound command's own children running past the deadline that was supposed to stop them. Don't drop `Setpgid`/the group kill when touching that code - it has a test (`TestRunCheck_TimeoutKillsChildProcesses`) that will catch it if you do.
- A green or red local run (issue #3) also has to land as a GitHub commit status (`internal/inspector/status.go`, wired from `cmd/inspector/main.go`) before the CLI is allowed to exit as that verdict - see SPEC.md section 7. The status context is the stable string `inspector`, which `inspector-gate` (#4) keys on; the token comes from `GITHUB_TOKEN`. A missing token, an unresolvable `origin` remote, or GitHub rejecting the post all fail loudly and exit `2`, even when the local check itself passed - a printed "green" is never proof the status reached GitHub. A `Refused` outcome posts nothing on purpose: absence already reads as failing, same as an unreachable API.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
