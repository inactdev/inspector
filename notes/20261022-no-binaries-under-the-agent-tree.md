# No binary file may live under the agent source tree

`internal/agentsource` is one rule with one reason (issue #36): nothing under
Inspector's agent source tree may contain binary content, and the project's own
check command is what refuses it.

The history is the argument. The first trusted runtime committed two gzipped
Linux agent binaries with their checksums as constants plus a script proving the
checksums matched, and it was replaced by `internal/examiner/runtime/Dockerfile`
building the agent from source. Nothing about that replacement prevents the
binaries returning: a reviewer looking at a `.gz` learns nothing, this is a
public repository, and the agent is the component whose entire job is refusing
to trust what it cannot verify. A ruling in conversation and a note in a README
had both already failed to hold once in that same piece of work, which is why
the rule is executable now.

## The decisions worth remembering

**Why Go test and not a shell script with its own CI job.** The repository's
other CI rules (`inspector-gate.sh`, `fabrica-contract-check.sh`,
`gate-sync.sh`) are shell plus a `_test.sh`, so shell was the obvious shape and
was rejected anyway. Content detection in shell means `grep -I`, `file`, or
`git`, which means new entries in `required-tools.sh` and a new tool the check
container's image must carry; `go test ./...` already runs in `script/check` on
both the host and CI, and Go reads bytes exactly. The rule lives where it costs
nothing new to run.

**Why the filesystem and not the git index.** The scan walks the directories
rather than asking git what is tracked. The thing being protected is the agent
image's build context, and `COPY` reads the filesystem: a `.gitignore`d binary
is still copied into the agent image, and inspector's own dirty-tree check also
allows ignored files. Scanning the index would miss exactly that file. The one
name skipped is `.DS_Store` - already gitignored, so uncommittable, and
ubiquitous on macOS; failing a developer's check run over Finder's metadata
teaches people to route around the rule.

**Why content and not extensions.** An extension list is a list somebody must
remember to extend, and it says nothing about a gzipped binary committed as
`agent-data` or `notes.txt`. Text here means valid UTF-8 with no C0 control
byte other than tab, newline, vertical tab, form feed and carriage return, and
no DEL. That rejects every executable, archive and image format while accepting
source in any language, and the failure names the offending byte and its offset
so nobody needs a hex dump to act on it. Files are streamed, so the size of
what somebody committed does not decide whether the rule can inspect all of it.

**Why the allowance is shaped the way it is.** A binary test fixture is a real
need, so there is one exception - `agentsource.Allowed`, empty today - and three
restrictions that `Scan` enforces rather than leaving to review: an entry names
one exact path (never a directory, prefix or pattern), it must sit under a
`testdata` directory so no allowance can ever cover code that ships inside the
agent, and it must still be earning its place. An allowance whose file is
absent, no longer binary, outside the guarded roots, or carries no stated reason
is itself a failure. A permission that outlives its reason is how a narrow
exception becomes a hole.

**Why a missing root is an error, not an empty pass.** `Roots` is named
explicitly rather than inferred, so a rename or a typo could silently point the
rule at nothing and still report green. `Scan` fails on a root it cannot stat,
and a test removes each root in turn to prove it.

**What the negative test is for.** This is a *never* ruling, so it ships with a
test that attempts the forbidden thing: one writes a gzipped file into the real
`internal/examiner/runtime/`, requires `Scan` to reject it, removes it, and
requires the tree to read clean again. A second mirrors the tree's shape in a
temporary directory and plants binary content under *every* root under the name
`notes.txt`. The first is the literal act; the second survives issue #27 making
source read-only during a check, which is when the first one skips.

## Two adjacent facts, left standing

- `Roots` includes `internal/llm` because `internal/examiner/agent.go` imports
  it, so it links into the agent. The runtime Dockerfile does **not** copy it,
  and that build context therefore cannot compile the agent today -
  reconstructing the `COPY` set by hand and building fails with
  `no required module provides package .../internal/llm`. `runtime_identity.go`'s
  embed list omits it as well, so the runtime fingerprint does not cover it
  either. Both predate issue #36 and are untouched by it.
- The rule covers the agent source tree only. `cmd/inspector/`,
  `internal/inspector/`, `internal/reviewer/`, `examples/`, `script/` and
  `.github/` can still carry a committed binary. That is the issue's scope, not
  an oversight: the agent is the part whose unreviewability is the whole point.
