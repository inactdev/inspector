---
project: inspector
commit: da1ce08e21bd73ea54e81b19bfb4b4e893ce95fe
vendor: hand-authored (no vendor credential available in this environment)
model: none
confirmed: no - replace this line with "confirmed: YYYY-MM-DD Your Name" once you have followed every command below yourself
---

# inspector: the examiner's guidebook

This file teaches Inspector's examiner how to operate this project. The
examiner is given this document and a running app, and never the source - so
anything it needs to know in order to drive a feature has to be in here.

**This guidebook is a draft until somebody confirms it.** Follow every command
below yourself, correct what is wrong, then replace the `confirmed` line in the
front matter with today's date and your name. Until that line names a date and
a person, an examination driven from this file records every capability as
could-not-be-tested rather than as a pass: nobody has vouched for these
instructions, so a response that arrives proves only that this draft's own
scenario ran.

The prose below is a model's reading of the facts in the last section. Those
facts were read out of the project's own files by Inspector, each with the line
it came from. When the two disagree, the facts are what happened.

## Launch

Inspector is a command-line program, not a server. There is no process to
start and no port to connect to: "launching" it means building the binary once
and then running it per judgment. Nothing listens, so do not look for a URL.

From a clean checkout, with Go 1.22 or newer on PATH. **Run the build from the
root of the checkout**, where `go.mod` is - from anywhere else it fails with
"go.mod file not found" and leaves no binary:

```
cd /path/to/this/checkout
go build -o /tmp/inspector ./cmd/inspector
/tmp/inspector --help
```

The second command prints usage and exits `64`. That is the correct exit code
for a help request and not a failure - see Evidence.

Two things it needs from the machine, neither of which the build provides:

- **A container runtime.** The project-check judgment runs the judged project's
  own check command inside a container and refuses outright rather than falling
  back to the host. Without a reachable Docker daemon, every project check
  refuses. `docker info` succeeding is the test.
- **A judged repository.** Inspector judges some other project's commit, so you
  need a git checkout to point it at. It refuses a dirty working tree.

The AI judgments additionally need a model credential in the environment, read
by name and never accepted on the command line.

```
export ANTHROPIC_API_KEY=...   # or <VENDOR>_API_KEY and <VENDOR>_BASE_URL
export GITHUB_TOKEN=...        # only for publishing a green project check
```

Read from: `f13` (cmd/inspector/main.go:1), `f14` (cmd/inspector/main.go:59), `f1` (.inspector.json:2), `f2` (.inspector.json:3), `f7` (go.mod:1)

## Doctor

There is no health endpoint. The equivalent is: does the binary run, and does
it refuse for the reason you expect?

```
/tmp/inspector --help; echo "exit=$?"
```

Healthy output is a usage block naming `--branch` and `--repo`, followed by
`exit=64`. Anything else - a missing binary, a linker error, exit `127` - means
the build did not produce a working program and nothing below is worth running.

Do not expect that usage block to list the subcommands: it describes the
project check only, and `examine`, `review` and `document` are not named in it.
Each has its own `--help`, for example `/tmp/inspector review --help`.

To check it can read a repository at all, point it at one and ask for a
judgment it will refuse cheaply:

```
/tmp/inspector --repo /path/to/some/checkout; echo "exit=$?"
```

Expect `exit=2` and a refusal naming the missing `--branch`. A refusal here is
the program working: it proves the binary runs, parses flags, and refuses
rather than guessing. An exit of `64` instead means the flags were not even
understood.

Read from: `f13` (cmd/inspector/main.go:1), `f14` (cmd/inspector/main.go:59)

## Drive

Everything is driven by running the binary with arguments and reading its exit
code, its stdout and its stderr. There is no HTTP interface, no socket and no
configuration dialogue: one invocation, one judgment, one exit code.

The first argument selects which judgment:

- no subcommand - the project check, which runs the judged project's own check
  command and, when green, publishes the result.
- `examine` - drive a running app and judge request-derived behavior.
- `review` - read a change as a colleague would and report what looks wrong.
- `document` - ask whether the change is described, and whether it made
  something already written untrue.

Every judgment takes its inputs as explicit flags. Inspector infers nothing
from the checkout: it will not work out which branch to publish, which commit
is being judged, or where the request text is. A missing input is a refusal
naming the flag, never a default.

Secrets are read from the environment by name. Do not pass a key as a flag
value; there is no flag that accepts one.

Each judgment prints a JSON report on stdout before its human-readable lines,
so a caller can read either.

Read from: `f11` (cmd/inspector/document.go:19), `f12` (cmd/inspector/examiner.go:21), `f14` (cmd/inspector/main.go:59), `f15` (cmd/inspector/reviewer.go:19)

## Evidence

**The exit code is the verdict, and it is not pass/fail.** Four values, and
confusing two of them is the main way a judge of this program gets it wrong:

- `0` - green. The judgment ran and passed.
- `1` - red. The judgment ran and failed. A real finding about the judged work.
- `2` - no verdict. A refusal, or a green result whose publication did not
  complete. **Never read this as red.** It says the question was not answered,
  not that the answer was no.
- `64` - not a verdict attempt at all: `--help`, an unknown flag, bad usage.

**stdout carries the record.** Each judgment prints an indented JSON object
first. For a project check it also prints `green - <commit>` or `red - <commit>`
with the path of the local report.

**stderr carries anything that must not be missed**, including refusals and
warnings fenced between two long lines of exclamation marks. A run that exits
`0` while printing a fenced warning is still a run you must read.

**On disk**, a project check writes one JSON report per run under `.inspector/`
in the judged repository, including `latest.json`. These are notes, never
authority: the real record of a green project check is a commit status posted
to GitHub under the context `inspector`, and the examiner's is a separate status
under `examiner`. A judgment you cannot see on the commit did not publish.

For a failed project check, the report holds the command, its output and the
verdict, so you can tell what failed without rerunning it.

Read from: `f1` (.inspector.json:2), `f58` (internal/inspector/status.go:44)

## Cleanup

Nothing keeps running after a judgment returns, so there is no process to stop.
What a run can leave behind:

- `.inspector/` in the judged repository: one JSON report per run. Safe to
  delete; the project's own `.gitignore` normally covers it.
- The built binary, wherever you put it.
- Containers, if a check command was killed in an unusual way. `docker ps` will
  show them.
- **Remote state, which you cannot undo.** A green project check stages a ref,
  posts a commit status and moves the named branch. A published status cannot be
  withdrawn by rerunning anything. Judge this against a throwaway repository or
  with no `GITHUB_TOKEN` set, never against a repository whose statuses matter.

Use a fresh copy of the judged checkout per judgment rather than reusing one: a
check command may leave untracked build output that makes the next run refuse on
a dirty tree.

Read from: `f1` (.inspector.json:2)

## Cannot be tested here

Report each of these as could-not-be-tested rather than as a failure of the
program.

- **Anything needing a container runtime.** Every project check runs inside a
  container by design and refuses rather than running on the host. With no
  usable Docker daemon, the entire project-check judgment is untestable - and
  the refusal you get is correct behavior, not a bug.
- **Anything needing a model credential.** `examine`, `review` and `document` all
  call a model vendor. Without a key in the environment they refuse by naming
  the missing variable. That refusal is the only observable behavior available.
- **Anything needing GitHub.** Staging a commit, posting a status and moving a
  branch all need a token and a reachable API. Without them the publication half
  of a green result cannot be reached at all, and a green check exits `2`.
- **The examiner's sealed runtime.** It must be built locally before an
  examination, and the examination itself deliberately never builds or pulls it.
  No Docker means no examination.
- **Whether a timed-out check's container was really killed.** The program
  reports what the runtime told it. Confirming the kill from outside needs
  access to the daemon you are judging through.
- **The gate.** The cloud half is a GitHub Actions workflow. It cannot be run
  from a command line; its own shell tests, which the project check runs, are
  the only local evidence about it.

Read from: `f2` (.inspector.json:3), `f3` (internal/examiner/runtime/Dockerfile:12), `f54` (internal/container/user.go:73), `f56` (internal/examiner/agent.go:22), `f58` (internal/inspector/status.go:44)

## Features

One entry per user-facing feature, each with the same four headings.

### Examining a running app against what was asked for

#### Sub-features

Deriving capabilities from the request text; driving an already-running HTTP app through a constrained driver; recording every attempt and delivered response per capability; deriving each per-capability verdict from that record rather than from the model's word; keeping a found bug separate from an incomplete examination; posting the separate `examiner` status; proposing a regression line for missing behavior.

#### How to get to it from the user's point of view

The app is started by the caller first, outside the examination. They then run the `examine` subcommand with the request text, the project's guidebook and always-true list, the changed-file names, the pre-task tests, and the address the app is already listening on.

#### Driving it with the harness

Build the sealed runtime once, then examine:

```
internal/examiner/runtime/build.sh
ANTHROPIC_API_KEY=... GITHUB_TOKEN=... /tmp/inspector examine \
  --request /tmp/request.md \
  --feature-map /tmp/guidebook.md \
  --always-true /tmp/always-true.md \
  --changed-files /tmp/changed-files.json \
  --base-tests /tmp/base-tests.json \
  --app-url http://host.docker.internal:8080 \
  --commit <40-character-sha> --owner <owner> --repo <repo> \
  --model <model-name>
```

Without a key, without Docker, or with any required flag missing, it refuses and exits `2` - which is the only thing observable in that environment.

#### Gotchas

The examiner never receives the repository, a diff, or worker-written file content, so asking it to read the code is asking for the one thing it is built to refuse. It never starts the app; an unreachable `--app-url` produces could-not-be-tested, not a bug. It never builds or pulls its runtime image during an examination. A guidebook whose `confirmed` line names no date and name makes every capability could-not-be-tested, so an examination that confirms nothing may be saying the guidebook is unsigned rather than that the app is broken - read the `guidebook:` line first. A found bug exits `1` and posts `failure`; an incomplete examination exits `2` and posts `error`; both facts are kept when both are true.

Read from: `f12` (cmd/inspector/examiner.go:21), `f29` (cmd/inspector/examiner.go:23), `f30` (cmd/inspector/examiner.go:24), `f31` (cmd/inspector/examiner.go:25), `f34` (cmd/inspector/examiner.go:28), `f56` (internal/examiner/agent.go:22)

### Finding the documentation a change made untrue

#### Sub-features

Asking whether the change is described where it belongs; asking the harder question of whether the change falsified something already written elsewhere; building a corpus from the project's tracked text and serving the model only from it; verifying every quoted claim back against the file; proposing a correction or flagging that the claim needs the owner's judgment; refusing to propose a document the project has never had.

#### How to get to it from the user's point of view

Somebody with a finished change wants to know what it quietly made wrong. They run the `document` subcommand with the change as a diff and the project to re-read, and optionally the request text that says what was meant.

#### Driving it with the harness

```
ANTHROPIC_API_KEY=... /tmp/inspector document \
  --diff /tmp/change.diff \
  --repo /path/to/judged/checkout \
  --request-file /tmp/request.md \
  --model <model-name>
```

The result's `corpusLookups` is how many times it opened the project; a completed review cannot have zero.

#### Gotchas

The stale document is usually not in the diff at all, which is the whole point - do not expect findings to land on changed files, and check each finding's `inChange` flag. It reports and never edits: a correction is proposed text. It exits `0` whether or not it found anything. Without `--request-file` it says it was told nothing about intent and flags anything needing intent for the owner instead of guessing. A finding naming a file outside the project's tracked text is rejected, so it can never propose that the project add a document it never had.

Read from: `f11` (cmd/inspector/document.go:19), `f24` (cmd/inspector/document.go:21), `f25` (cmd/inspector/document.go:22), `f26` (cmd/inspector/document.go:23)

### Judging whether a finished commit passes the project's own checks

#### Sub-features

Reading the judged project's `.inspector.json` for its check command and container image; running that command inside a resource-bounded, network-denied container as the inviting host user; mounting the commit under check read-only with a throwaway writable workspace in its place; killing a check that exceeds its timeout; writing one local JSON report per run; refusing when there is no check command, no image, no container runtime, or a dirty working tree.

#### How to get to it from the user's point of view

Somebody believes a piece of work is finished and hands it over. They run the program against that checkout with no subcommand. There is no screen and nothing to click: the judgment is the invocation, and the exit code is the answer.

#### Driving it with the harness

Run the binary against a judged checkout, naming the branch it may publish:

```
/tmp/inspector --repo /path/to/judged/checkout --branch some-pull-request-branch
```

The judged checkout must contain a committed `.inspector.json` declaring both a `check` command and an `image`, and its working tree must be clean. Expect `0` with `green - <commit>`, `1` with `red - <commit>`, or `2` with a refusal naming what was missing.

To see a refusal without needing a container runtime at all, point it at any existing git checkout that has commits and no `.inspector.json`. Expect `exit=2` and `refused: no check command configured for this repo`:

```
/tmp/inspector --repo /path/to/any/other/checkout --branch anything; echo "exit=$?"
```

Use a checkout that already exists rather than creating one: a brand new `git init` has no commits, so it refuses one step earlier about being unable to resolve HEAD, and committing to it first needs a configured git identity.

#### Gotchas

`--branch` is required and is never inferred, and naming the remote default branch is refused. A clean working tree is required, so a check command that leaves untracked build output can make the next run refuse. `2` means no verdict, not red. Green does not stop at the local check: it then stages the commit, posts a status and moves the branch, so a run with no `GITHUB_TOKEN` can pass the check and still exit `2` as an incomplete publication - which is not a failure of the work. A red result publishes nothing at all by policy, so finding nothing on GitHub after a red run is correct.

Read from: `f1` (.inspector.json:2), `f2` (.inspector.json:3), `f43` (cmd/inspector/main.go:61), `f44` (cmd/inspector/main.go:62), `f8` (script/check:1)

### Publishing a green result where a merge gate can see it

#### Sub-features

Staging the checked commit on a non-branch remote ref; posting the `inspector` commit status; posting the separate changed-test status when the project declares test paths; moving the named pull request branch, fast-forward only unless a rebase is explicitly authorized; opening or finding a pull request; deleting the temporary staging ref.

#### How to get to it from the user's point of view

It is not a separate command. A person gets here by handing over work that passes: publication is what a green project check does next, in a fixed order, so a gate never sees a moved branch without a result attached to its commit.

#### Driving it with the harness

Add the publication flags to a green project check:

```
GITHUB_TOKEN=... /tmp/inspector \
  --repo /path/to/judged/checkout \
  --branch some-pull-request-branch \
  --expected-tip <40-character-sha> \
  --pr-title "Title" --pr-body-file /tmp/body.md
```

`--expected-tip` is only needed when the branch's history was rewritten locally. `--pr-title` and `--pr-body-file` go together; either alone is a refusal.

#### Gotchas

Judge this against a throwaway repository only: a posted status cannot be withdrawn. Steps are deliberately not compensated - a failure after the status was posted leaves partial remote state and says so rather than tidying up. A status request whose response was lost is reported as "stamp sent, outcome unconfirmed" and is neither green nor not-attempted. Giving `--pr-title` without `--pr-body-file` exits `2` before anything runs. A branch already pointing at the checked commit is refused rather than retroactively stamped.

Read from: `f44` (cmd/inspector/main.go:62), `f45` (cmd/inspector/main.go:63), `f46` (cmd/inspector/main.go:64), `f47` (cmd/inspector/main.go:65), `f58` (internal/inspector/status.go:44)

### Reading a change the way a colleague would

#### Sub-features

Annotating the supplied diff with real new-file line numbers before the model sees it; rejecting a finding that names a file outside the diff; judging, with `--test-paths`, whether a changed or removed test weakened the exam the change has to pass; reporting without posting a status or gating a merge.

#### How to get to it from the user's point of view

A person who has a change in front of them and wants a second opinion runs the `review` subcommand with that change as a unified diff. Nothing is read from the repository: the diff is the whole input.

#### Driving it with the harness

Produce a diff and hand it over:

```
git -C /path/to/judged/checkout diff main...HEAD > /tmp/change.diff
ANTHROPIC_API_KEY=... /tmp/inspector review \
  --diff /tmp/change.diff \
  --test-paths '*_test.go' \
  --model <model-name>
```

#### Gotchas

It reports and never gates: it posts no status and exits `0` even when it found something, so reading the exit code alone tells you nothing about what it said - read the JSON. A finding pointing outside the supplied diff is rejected rather than recorded, so a thin diff produces a thin review. Given no `--test-paths`, it says outright that it judged no tests rather than implying the tests were fine.

Read from: `f15` (cmd/inspector/reviewer.go:19), `f50` (cmd/inspector/reviewer.go:21), `f51` (cmd/inspector/reviewer.go:22), `f52` (cmd/inspector/reviewer.go:23)

## Facts Inspector extracted

These are the facts Inspector read out of the project's own files, with the
place each one came from. Everything above this section is a model's reading of
them; everything in it is quoted text. Check these first.

Read at commit `da1ce08e21bd73ea54e81b19bfb4b4e893ce95fe`, across 72 scanned file(s).

**check command**

- `f1` `the project's own check command is: script/check` - `.inspector.json:2`

**container image**

- `f2` `checks run in the image cimg/go:1.22` - `.inspector.json:3`
- `f3` `FROM golang@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS build` - `internal/examiner/runtime/Dockerfile:12`
- `f4` `WORKDIR /src` - `internal/examiner/runtime/Dockerfile:13`
- `f5` `FROM alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc` - `internal/examiner/runtime/Dockerfile:23`
- `f6` `ENTRYPOINT ["/usr/local/bin/examiner-agent"]` - `internal/examiner/runtime/Dockerfile:25`

**manifest**

- `f7` `Go module github.com/inactdev/inspector` - `go.mod:1`

**run script**

- `f8` `script/check (#!/usr/bin/env bash)` - `script/check:1`

**cli entry point**

- `f9` `command line: go run ./cmd/examiner-agent` - `cmd/examiner-agent/main.go:1`
- `f10` `command line: examiner-agent` - `cmd/examiner-agent/main.go:26`
- `f11` `command line: inspector document` - `cmd/inspector/document.go:19`
- `f12` `command line: inspector examine` - `cmd/inspector/examiner.go:21`
- `f13` `command line: go run ./cmd/inspector` - `cmd/inspector/main.go:1`
- `f14` `command line: inspector` - `cmd/inspector/main.go:59`
- `f15` `command line: inspector review` - `cmd/inspector/reviewer.go:19`

**command flag**

- `f16` `--input-dir` - `cmd/examiner-agent/main.go:28`
- `f17` `--output-dir` - `cmd/examiner-agent/main.go:29`
- `f18` `--app-url` - `cmd/examiner-agent/main.go:30`
- `f19` `--vendor` - `cmd/examiner-agent/main.go:31`
- `f20` `--model` - `cmd/examiner-agent/main.go:32`
- `f21` `--timeout` - `cmd/examiner-agent/main.go:33`
- `f22` `--budget` - `cmd/examiner-agent/main.go:34`
- `f23` `--runtime-fingerprint` - `cmd/examiner-agent/main.go:35`
- `f24` `--diff` - `cmd/inspector/document.go:21`
- `f25` `--repo` - `cmd/inspector/document.go:22`
- `f26` `--request-file` - `cmd/inspector/document.go:23`
- `f27` `--vendor` - `cmd/inspector/document.go:24`
- `f28` `--model` - `cmd/inspector/document.go:25`
- `f29` `--request` - `cmd/inspector/examiner.go:23`
- `f30` `--feature-map` - `cmd/inspector/examiner.go:24`
- `f31` `--always-true` - `cmd/inspector/examiner.go:25`
- `f32` `--changed-files` - `cmd/inspector/examiner.go:26`
- `f33` `--base-tests` - `cmd/inspector/examiner.go:27`
- `f34` `--app-url` - `cmd/inspector/examiner.go:28`
- `f35` `--commit` - `cmd/inspector/examiner.go:29`
- `f36` `--owner` - `cmd/inspector/examiner.go:30`
- `f37` `--repo` - `cmd/inspector/examiner.go:31`
- `f38` `--vendor` - `cmd/inspector/examiner.go:32`
- `f39` `--model` - `cmd/inspector/examiner.go:33`
- `f40` `--network` - `cmd/inspector/examiner.go:34`
- `f41` `--timeout` - `cmd/inspector/examiner.go:35`
- `f42` `--budget` - `cmd/inspector/examiner.go:36`
- `f43` `--repo` - `cmd/inspector/main.go:61`
- `f44` `--branch` - `cmd/inspector/main.go:62`
- `f45` `--expected-tip` - `cmd/inspector/main.go:63`
- `f46` `--pr-title` - `cmd/inspector/main.go:64`
- `f47` `--pr-body-file` - `cmd/inspector/main.go:65`
- `f48` `--vendor` - `cmd/inspector/main.go:66`
- `f49` `--model` - `cmd/inspector/main.go:67`
- `f50` `--diff` - `cmd/inspector/reviewer.go:21`
- `f51` `--test-paths` - `cmd/inspector/reviewer.go:22`
- `f52` `--vendor` - `cmd/inspector/reviewer.go:23`
- `f53` `--model` - `cmd/inspector/reviewer.go:24`

**environment variable**

- `f54` `DOCKER_CONTEXT` - `internal/container/user.go:73`
- `f55` `DOCKER_HOST` - `internal/container/user.go:75`
- `f56` `ANTHROPIC_API_KEY` - `internal/examiner/agent.go:22`
- `f57` `XDG_CONFIG_HOME` - `internal/inspector/repo.go:906`
- `f58` `GITHUB_TOKEN` - `internal/inspector/status.go:44`

Not scanned for routes, commands or flags:

- test files, whose routes and commands are fixtures rather than things the app serves
- vendored, generated and fixture directories (testdata)
