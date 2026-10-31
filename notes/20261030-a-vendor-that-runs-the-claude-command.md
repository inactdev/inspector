# A vendor that runs the claude command (issue #70)

Issue [#70](https://github.com/inactdev/inspector/issues/70): at the day job the
only model access is Claude Code on a company subscription - no API key, no
OpenRouter. The test reviewer has to run there anyway, so `internal/llm` grows a
third vendor, `claude-code`, whose `Adapter` runs the `claude` command on the
host instead of posting to an HTTP endpoint. It needs no `<VENDOR>_API_KEY` and
no `<VENDOR>_BASE_URL`, because the command carries its own login.

## What the command is actually asked

One `Ask` is one `claude --print` run, fed the whole conversation on stdin:

    claude --print --output-format json --model <model>
           --tools "" --safe-mode --no-session-persistence
           --strict-mcp-config --disable-slash-commands
           --json-schema <the block schema> --system-prompt <caller's system>
           --append-system-prompt <the block protocol>

and the blocks come back out of the envelope's `structured_output` field.

Four of those flags are the containment, and none of them is decoration:

- `--tools ""` disables every built-in tool. The caller's tools
  (`report_findings`, `judge_test_change`, ...) are Inspector's own and are
  answered by Inspector; Claude Code's `Bash`, `Edit` and `Write` must never
  run at all. This is what keeps SPEC.md section 4 true - a judgment that
  could edit the judged project is not a judgment.
- `--safe-mode` disables the host's customizations: hooks, `CLAUDE.md`,
  skills, plugins, MCP servers, custom agents. Hooks matter most. Auth and
  model selection keep working, which is the whole reason this flag and not
  `--bare`: `--bare` reads *only* `ANTHROPIC_API_KEY` and never OAuth or the
  keychain, which is exactly the credential this vendor exists to use.
- `--strict-mcp-config` with no `--mcp-config` leaves no MCP server, so no
  tool can arrive that way either.
- `--disable-slash-commands`, because the prompt is a diff written by whoever
  wrote the change under review, and a line in it that starts with `/`
  must stay text.

The run also happens in a throwaway empty directory rather than the judged
repository, so nothing is read from, or written into, the project's tree - and
`--no-session-persistence` keeps the judged change out of the session history
on disk.

## Why a rendered protocol rather than real tool calls

`claude --print` runs its own agent loop with its own tools; there is no flag
that hands it a caller's tool definitions. So the adapter renders the tools and
the conversation into one prompt and asks for a JSON object naming which tools
the model wants called - one `{"type":"tool_use","tool_name":...,"tool_input":...}`
block per call - validated by `--json-schema` on the way out. The adapter mints
the `tool_use` IDs itself (`claude-code-<n>`, n counted from the tool calls
already in the conversation) rather than letting the model name them, so an
omitted or colliding ID is not a failure mode at all.

Nothing downstream changes shape: the same `[]llm.Block` the Anthropic and
OpenAI adapters return, so findings, the `inspector-tests` status and the run
report are byte-identical to an API-key run.

**Rejected: lift the subscription's OAuth token and keep speaking the Messages
API.** The adapter could read Claude Code's stored credential and reuse
`AnthropicAdapter`, which would keep native tool calling and cost no rendering
at all. It was rejected because it treats another product's credential store as
Inspector's: it reads a token Inspector was never given, sends it to an endpoint
the subscription does not license, and breaks the first time that storage
changes. Shelling out to the documented non-interactive interface keeps
Inspector's hands off the credential entirely - nothing is read, stored,
logged, or forwarded.

**Rejected: serve the caller's tools over MCP.** `--mcp-config` plus a small
Inspector-run MCP server would give real tool calls. It means Inspector running
a server and granting tool permissions to a full coding agent on the host, to
obtain data the structured-output protocol already returns with no server, no
permissions, and no tools enabled.

## The examiner cannot have this vendor

`internal/examiner` is the one caller that does not get it for free, and it
refuses `claude-code` by name instead. Its agent runs *inside* the sealed
runtime, which carries no `claude` command and no login for one; the only ways
to change that are to mount the host's `claude` and its credential into the
sealed container, or to rebuild the pinned runtime around it. Both put a full
coding agent and an OAuth credential inside the one component whose entire
purpose is a sealed runtime - strictly worse than the single forwarded
environment-variable name it uses today. The refusal is raised before any
container starts, so the cost is a clear sentence rather than a ten-minute
examination that fails obscurely.

That refusal travels the path every other examiner refusal already does: the
separate `examiner` status goes to `error` ("could not be tested") and
`inspector examine` exits `2`, exactly as it does today for a missing
`ANTHROPIC_API_KEY`.

The code reviewer, the test reviewer in `inspector run`, the documentation
judgment and the guidebook all run on the host and do get it for free.

## Where a missing `claude` surfaces

`llm.Resolve` looks the command up on `PATH` before returning the adapter, so
the failure is named once, in the one place every caller already handles a
vendor it cannot reach:

- `inspector review`, `document` and `guidebook` refuse, naming `claude`, and
  exit `2`.
- `inspector run` posts `inspector-tests: failure` naming it, and does not
  fail the run - `cmd/inspector/testchanges.go` is deliberately incapable of
  turning a judgment problem into a refusal, because a refusal would suppress
  the publication the gate needs in order to see any status at all. A missing
  `claude` is handled there exactly as a missing `ANTHROPIC_API_KEY` already
  is.
