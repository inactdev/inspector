# Generating the examiner's guidebook (issue #22)

Issue #22 is **the generator, not any one project's guidebook**. The examiner
(#7) is denied the repository on purpose, so something that *is* allowed to read
the repository has to convert code knowledge into operating instructions once,
at setup time. That is `inspector guidebook`.

## The design, decided before writing any of it

**Facts first, then prose, and the facts stay in the output.** Inspector
extracts deterministic facts itself - the check command and image from
`.inspector.json`, manifests, run scripts, compose services and published
ports, Dockerfile `EXPOSE`/`ENTRYPOINT`/`CMD`, HTTP route registrations, CLI
entry points, environment variables, listen addresses - each one carrying the
`path:line` it was read from. The model then writes the guidebook *grounded on
those facts*, and the rendered document ends with every fact listed verbatim.
A reader can therefore tell fact from inference without opening the repository,
which is the only way a confirming human can confirm anything.

**The model proposes; inspector validates and renders.** Same division as the
documenter and the reviewer. The model can search, read, and list the project's
tracked text (the documenter's corpus, reused rather than given a second
dialect), then proposes sections and features through tools. Inspector rejects
a proposal that is missing a required section, a feature missing one of its
four headings, a `groundedIn` fact id that does not exist, a `Launch` or
`Doctor` section with no runnable command in it, or a finish called before
anything in the project was read. The markdown is rendered by inspector's own
renderer from validated data, so the model never decides the document's shape.

**One file, at the target repo's root, never clobbered.** `INSPECTOR-GUIDEBOOK.md`
beside `.inspector.json`, committed, with sections `Launch`, `Doctor`, `Drive`,
`Evidence`, `Cleanup`, `Cannot be tested here`, then `Features` (one entry per
user-facing feature with exactly four headings: sub-features, how to get to it
from the user's point of view, driving it with the harness, gotchas), then the
extracted facts. When the real file already exists, the generator writes
`INSPECTOR-GUIDEBOOK.draft.md` beside it and says so; the real file is created
with `O_EXCL` and is never written over, so a human-edited guidebook cannot be
lost to a second run.

**Unconfirmed is a fact the examiner acts on.** The front matter carries
`confirmed: no - ...` until a human replaces that line with
`confirmed: YYYY-MM-DD Name`. The examiner parses that line and, for an
unconfirmed guidebook, derives `could_not_be_tested` for every capability that
would otherwise have been confirmed, marks the examination incomplete, and
posts `error`. A found bug stays red: the safe direction is blocking, not
passing. A feature map with no guidebook front matter at all is a hand-written
document a human already wrote and committed, so it behaves exactly as before -
and the result says which of the two it was, so the absence is never silent.

## The alternative I rejected

**Deterministic templates only, with no model pass.** Parse the manifests,
routes, compose files and CLI entry points and emit the guidebook straight from
them. It is reproducible, needs no vendor credential, and cannot hallucinate - a
genuinely attractive safety story.

Rejected because the guidebook's whole job is the thing a template cannot do.
"What the app does in a user's terms, not a module list" is a translation, and
so is "how to get to it from the user's point of view". A template that has
`POST /inklings` can only ever emit `POST /inklings`; it cannot say that this is
how a thought gets captured, that an id repeated is an update rather than a
second record, or that the audio part is optional. An examiner handed a route
table still has to guess, which is the failure mode #22 exists to remove. The
safety the template bought is recovered a different way: every deterministic
fact is extracted by inspector and printed verbatim in the output, the prose
must point at those facts by id, and no guidebook is authority until a human
signs the `confirmed:` line.

## What running it on an unseen project taught, keep this

Pointing the extractor at a project nobody had tried it on is what found both of
its real defects, and neither was findable from a fixture somebody wrote while
writing the rule:

- **A compose file's services are the keys at one exact indent level under
  `services:`.** Taking any indented key reported `command`, `ports` and
  `environment` as services of their own.
  `TestExtractFacts_ReadsOnlyTheTopLevelComposeKeysAsServices` holds this.
- **A quoted `name:number` is usually an image tag, not an address.**
  `"node:20"` and `"postgres:16"` were being listed as listen addresses, and a
  guidebook naming port 20 is worse than one naming no port. The host half now
  has to look like a host: empty, `localhost`, an IP, or something dotted.

Two limits of literal extraction worth knowing before extending it. A variable
read through a named constant - `os.Getenv(GitHubTokenEnvVar)` - is invisible to
a rule matching literals, which is why there is a narrow rule for
`...Env... = "SHOUTING_NAME"` as well; `GITHUB_TOKEN` and `ANTHROPIC_API_KEY`
only appear because of it. And the corpus is tracked files only, so a command
added in the same change that generates the guidebook is not in it.

## Two things found while doing this, deliberately not changed

- `internal/examiner/runtime/Dockerfile` copies `cmd/examiner-agent`,
  `internal/container` and `internal/examiner` into the build context but not
  `internal/llm`, which `internal/examiner/agent.go` has imported since the
  vendor seam landed (issue #42). `go build ./cmd/examiner-agent` inside that
  context cannot resolve it. `runtime_identity.go`'s `//go:embed` list has the
  same gap, so the fingerprint that is supposed to identify the agent's source
  omits a package compiled into it. Both are outside issue #22 and neither is
  verifiable without a Docker daemon, so they are reported rather than patched.
- The confirmation contract therefore lives in `internal/examiner`
  (`guidebook.go`) and the generator imports it, rather than the other way
  round. A new package in the agent's import graph would have needed that
  Dockerfile and that embed list changed to ship at all; this way the sealed
  agent's dependency footprint is unchanged and there is still only one parser.
