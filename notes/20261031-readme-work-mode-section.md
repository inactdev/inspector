# README's Work mode section (issue #78)

Issue [#78](https://github.com/inactdev/inspector/issues/78), documentation
only. Written 2026-10-31.

`-terms` and `-no-stamp` (#69) and `-vendor claude-code` (#70) each had their
own README section, and nothing said when and how the three are used together.
`## Work mode` is now that one place: the invocation, what the terms folder
holds, what the report and stdout say when no stamp is posted, and the single
command that refuses the vendor.

## Where each fact in it comes from

Nothing in the section is new behavior, and nothing was taken from the issues'
prose. Each claim has a source in the code, which is where to check it if a
later change makes the section stale:

- the invocation's non-work-mode flags - `cmd/inspector/testdata/fabrica-consumer-contract.json`,
  the consumer contract `TestFabricaConsumerContract` checks against, which is
  Fabrica's own record of how it calls inspector.
- `GITHUB_TOKEN` still required under `-no-stamp` - `publishGreen` in
  `cmd/inspector/main.go` reads it before any step, stamp policy or not.
- the model flags mattering only for a `testPaths` project - `internal/inspector.Run`
  takes `cfg` from `Terms.Config()`, so in this mode `testPaths` comes from the
  terms folder's `.inspector.json`, not the repository's.
- the stdout lines and the report's `termsDir` / `noStamp` - `cmd/inspector/main.go`
  (`noStampPolicy`, `publishedSoFar`, `printTestJudgment`) and
  `internal/inspector/report.go`.
- the examiner's refusal, its `error` status and its exit `2` -
  `internal/examiner/container.go` and `postExaminerStatus` in
  `cmd/inspector/examiner.go`.

## One heading was renamed

The reviewer's `### Work mode: --vendor claude-code` became
`### The claude-code vendor`, and its two inbound links were updated with it.
Two headings called "Work mode" - one a top-level section about three flags,
one a subsection about a vendor - would have been the kind of near-duplicate
that sends a reader to the wrong one. The mode owns the name; the vendor
section is named for the vendor, which is what it documents, and it is still
where the containment flags of the `claude` run are explained.

Anchor churn is the cost: `#work-mode---vendor-claude-code` no longer
resolves. It was linked from two places in this README and nowhere else in the
repository.
