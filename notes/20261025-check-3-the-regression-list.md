# Check 3: the regression list, and why it brings the app up itself

Issue [#24](https://github.com/inactdev/inspector/issues/24), as rescoped by the
2026-10-08 Client ruling. This note records the decisions a later agent cannot
re-derive from the code, and the two alternatives that were considered and
rejected.

## The artifact

One file, one name, one check. The ruling settles that this issue's
"invariants" document and the always-true list the examiner already takes as
`--always-true` are the **same artifact**, so there is exactly one format and
`internal/alwaystrue` owns it:

- Default path `.inspector-always.json` at the repo root, overridable with
  `--list`. Named to match the status context `inspector-always`, and to sort
  beside `.inspector.json` so a reader finds both at once. The two files are
  not the same kind of thing: `.inspector.json` is the rules of the game and
  stays human (fabrica#97 item 4), while this list is accumulated knowledge
  about the product and has to be able to grow.
- Each line carries **both halves**: a plain-language `promise` a person can
  read, and the `falsify` command, driver script, or HTTP exchange that would
  disprove it. That is what reconciles the pre-rescope text ("plain language,
  not test code") with the ruling ("all of them must still pass"): the prose is
  what the list is *for*, the falsifier is what makes it a check.
- A line with no `falsify` is **not-runnable**, and not-runnable is never
  reported as passed. A declared-but-empty `falsify` is refused at parse time
  instead, because an empty falsifier is the silent weakening the whole issue
  exists to prevent.
- `confirmed` records whether a person has accepted the line. It is reported
  and counted, and it deliberately does **not** change the verdict: a broken
  promise nobody accepted yet is still a broken promise, and the owner can see
  from the count which lines were proposals.

## Why it brings the app up itself

The examiner is handed `--app-url` for an app the caller already started. Check
3 is not: it starts the app from the image the list names, seeds the fixture
data the list declares, waits for the declared readiness response, and removes
the container afterwards.

The reason is that check 3's whole claim is "all of these still pass". A
caller-supplied URL could point at anything - a long-lived instance whose
accumulated state makes a promise pass vacuously, or a different build
entirely. A promise is only evidence about *this* commit if the thing it was
driven against was brought up, from a declared image, with known data, for that
one run.

**Rejected alternative: take `--app-url`, like the examiner.** Cheaper to build
and it would have reused the examiner's own interface verbatim. Rejected for
the reason above - and because tearing the app down is what makes the fixture
data trustworthy on the *next* run too.

## How the falsifiers reach the app, without a repository mount

Read-only holds by construction rather than by instruction: **check 3 mounts no
repository**, the same boundary the examiner has. So there is nothing to prove
afterwards about what it edited - it had no handle on the commit at all. The
consequence to understand before changing this: a driver script is named by its
path **inside the runner image**, never by a path in the repo, because the repo
is not there. That is the same direction issue #47 is already pushing projects
in (commit your own image with what the checks need baked in).

The app container publishes its port on `127.0.0.1` with an ephemeral host port
(`-p 127.0.0.1:0:<port>`), which Inspector reads back with `docker port`. HTTP
falsifiers are then performed by Inspector itself from the host, so the
deterministic shape needs no shell and no extra image. Command and script
falsifiers run in their own single-use container joined with
`--network container:<app>`, which shares the app container's network stack, so
they reach the app on `127.0.0.1:<port>` - Docker's own documented sidecar
pattern, and the reason `APP_URL` inside a falsifier container names the
container port rather than the published one. `--publish` is not a legal flag
on a container in that mode; publishing on the app container covers the group.

**Rejected alternative: a per-run user-defined bridge network** with both
containers attached and the app reached by container alias. It works, but it is
a third Docker object to create and tear down, it needs its own failure
handling, and it buys nothing over sharing the app's netns.

## Verdict shape

One aggregate, never fail-fast: every promise is attempted even after one
breaks, so the owner reads the whole list's state from a single status.

- `success` - every promise held and every one of them ran.
- `failure` - at least one promise broke. Names all of them. A broken promise
  outranks incompleteness, the same precedence the examiner uses for a
  confirmed bug.
- `error` - nothing broke, but something could not be run: the app would not
  start, or a line has no falsifier. Says which. Distinct from `failure`
  because "a promise broke" and "I could not check" are different facts.

Status context `inspector-always`, exit `0`/`1`/`2` mirroring the examiner.

## What this delivery deliberately does not do

- **It does not judge changes to the list.** Per the ruling a change is visible
  and judged against the request, not forbidden - but that judgment lives
  outside check 3, in the protected-path gate plus Inspector's evidence
  judgment, exactly as the test-change rule does (`protectedPaths` plus
  `testPaths` plus the `inspector-tests` status). Issue #24's "Done when" lists
  it; it is a follow-up, as is the pipeline that turns a confirmed check-2
  finding into a proposed line.
- **It is not wired into `inspector run` or the gate.** It is its own command
  and its own status, the same way `inspector examine` is.
- **It does not touch `.inspector.json`.** The list has to be named under that
  file's `protectedPaths` to be protected at all, and that entry is the one
  thing this delivery cannot add for itself: the config is the rules of the
  game and stays human (fabrica#97 item 4). It is carried as a proposal in the
  pull request description instead.

  Which is also the sharp edge to know when a config key is retired in code:
  removing the key has to be paired, in the same change, with removing it from
  *this* repository's own `.inspector.json`. `Load` refuses a retired key by
  name rather than ignoring it, so a config left naming one is refused before
  any check runs - and because a worker branch may not edit that file, every
  branch rebased onto such a default branch is blocked until a human removes
  the key. Issue #47's `image` and `network` are the first two keys this
  applies to.
- Inspector's own `.inspector-always.json` is seeded with lines inferred from
  its tests and every one of them is `confirmed: false` with no falsifier, so
  Inspector's own `inspector-always` run reports `error` and names each line as
  not-runnable. That is the honest reading, not a gap to paper over: Inspector
  is a CLI with no HTTP app to bring up, and check 3's no-repository-mount
  boundary is precisely what keeps those lines unrunnable today.
