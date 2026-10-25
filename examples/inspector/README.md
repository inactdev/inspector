# A generated guidebook, and the trial that corrected it

`INSPECTOR-GUIDEBOOK.md` here is the issue #22 proof artifact: the guidebook
`inspector guidebook` produces, for Inspector itself, at commit
`da1ce08e21bd73ea54e81b19bfb4b4e893ce95fe`.

It is kept here rather than at the repository root on purpose. It is a dated
artifact of one commit, not this project's live guidebook; running
`inspector guidebook` at the root is what writes that, and it would then cover
the `guidebook` command itself, which did not exist at the commit above.

Inspector is deliberately the hard case to point this at. It has no HTTP
interface at all - no port, no routes, nothing to start - so a generator that
only knew how to describe web apps would have had nothing to say. The whole
document is commands and exit codes.

## How it was produced

Every stage is the real one except the prose: Inspector extracted the 58 facts
and their lines, validated each section and feature against them, and rendered
the markdown. The prose itself was written by hand through the same validated
code path, because no model vendor credential is reachable from the environment
this was built in. The front matter says so - `vendor: hand-authored` - rather
than naming a model that never ran.

The facts half needs no credential at all and can be re-run against anything:

```
inspector guidebook --facts-only --repo /path/to/any/checkout
```

## The trial, and what it found

The point of a guidebook is that somebody who cannot read the code can follow
it. So every command in Launch, Doctor and the features was run as written, and
each claim checked against what actually happened. The run was not source-blind
- the same hands wrote the document - so it proves the instructions work, not
that they are discoverable without the code.

Confirmed exactly as written: the build and `--help` (usage, then exit `64`);
`--repo` with no `--branch` refusing with exit `2`; an unconfigured checkout
refusing with `no check command configured` and exit `2`; `examine` refusing
with exit `2` when its prerequisites are absent; `review` and `document`
refusing by naming `ANTHROPIC_API_KEY` with exit `2`.

Three things were wrong, and the document above is the corrected version:

1. **Doctor claimed the usage block names the subcommands.** It does not - it
   describes the project check only. Following that claim, a judge would have
   concluded the binary was broken. Now the document says outright that
   `examine`, `review` and `document` are absent from it and have their own
   `--help`.
2. **Launch did not say where to run the build from.** Run from anywhere but the
   checkout root it fails with "go.mod file not found" and leaves no binary, so
   a literal follower stops at step one with a missing-file error. Now it says
   `cd` first.
3. **The cheap-refusal example used a freshly created empty repository.** That
   refuses one step earlier, about resolving HEAD, and committing to it first
   needs a configured git identity - so the example produced neither the
   refusal it promised nor a usable repository. Now it points at an existing
   checkout and explains both traps.

Running the generator against a project nobody had tried it on found a real
defect in the generator itself, too: every key a compose file's service
declares is indented under it, and the first version read `command`, `ports`
and `environment` as services of their own.
`TestExtractFacts_ReadsOnlyTheTopLevelComposeKeysAsServices` holds that fixed.

## It is still unconfirmed, deliberately

Its `confirmed` line names no date and no name, so an examination driven from
it records every capability as `could_not_be_tested` rather than as a pass.
That is not an oversight in this example: confirming a guidebook is a person's
act, and nothing Inspector generates is authority until somebody signs for it.
