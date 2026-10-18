# Publishing a rebased pull request branch (`--expected-tip`)

Inspector's final publication step moved the named pull request branch with a
plain `git push`, so it was fast-forward only. That is the right default and
was never stated as a choice anywhere - it was simply what `PushRefToRemote`
did for all three of its callers. Fabrica then started rebasing a task branch
onto a moved base (fabrica#235), which produces a commit the remote branch does
not contain, and the failure was ugly in the specific way this repository cares
about: the green status was already recorded on the commit, the branch push was
rejected as non-fast-forward, and the run ended as an incomplete publication
with remote state half-written. Seen live on fabrica task
`20261018-do-fabrica-issue-186-as-written-closing-l7`.

## Why the caller names the commit

`--expected-tip <sha>` is deliberately caller-supplied rather than read by
inspector. Inspector could trivially `ls-remote` the branch and lease against
whatever it found, and every rebase would then publish with no flag at all -
but that turns a caller mistake into silent history loss. A mistyped
`--branch`, or a human who pushed to the branch meanwhile, would be overwritten
because inspector observed it a second earlier, which is not the same thing as
anyone having accounted for it. The flag is the caller saying *this* is the
history I am replacing; anything else there is work nobody accounted for, and
the run refuses.

Same reasoning rejects the two neighbouring git options: plain `--force` keeps
no lease at all, and `--force-if-includes` can never pass, because a rebased
commit by construction does not include the remote tip.

## The guard is in two places on purpose

- **Before any remote write**, `publishGreen` compares the branch's actual
  remote commit (the `ls-remote` it already performed to refuse a retroactive
  stamp) against `--expected-tip`. A mismatch, or a branch that is not there at
  all, is a *refusal*: nothing staged, nothing stamped, and a message naming
  both commits. This is the clear-message half.
- **On the push itself**, `--force-with-lease=refs/heads/<branch>:<sha>`. This
  is the half that actually holds, because it is the remote that arbitrates,
  closing the window between that read and the push. Verified against git
  2.54: the expected commit does **not** need to exist in the pushing
  repository's object database, which matters here - inspector pushes from a
  temporary bare repo with the real repo's objects as alternates, and the
  pre-rebase commit is unreferenced there.

A lease rejection is reported as **certain**, not unknown, which is otherwise
rare in this codebase: everywhere else a failed remote operation may mean a
lost response. git's `--porcelain` output carries the remote's own
`! <src>:<dst> [rejected] (stale info)` line, and only the remote can produce
it, so finding it proves the push was answered and the branch did not move.
That line is on **stdout**, not stderr, which is why `gitOutput` now returns
git's stdout alongside a failure as well as a success; `ForcePushRefWithLease`
is the only caller that reads it in that state. Its absence falls back to the
honest "outcome unknown" wording, so the detection can only improve a message,
never weaken one.

## Why the value is validated here rather than by git

`ValidateExpectedTip` accepts only a complete 40- or 64-character lowercase hex
object name. git already rejects an abbreviation (`cannot parse expected object
name`, exit 129), but it rejects it *after* inspector has committed to the
push, and a value that is not an object name at all could be read as an option.
Refusing it at flag-parsing time means the lease a caller asked for is always
the lease git enforces.

## The consumer contract

`cmd/inspector/testdata/fabrica-consumer-contract.json` gained an
`expected-tip` entry. That file is a byte-for-byte copy of Fabrica's own
`src/inspector/consumer-contract.json`, cross-checked live by CI's
`fabrica-contract` job, so it stays red until Fabrica's copy carries the same
entry - fabrica#235's own half of this change. See
[CLAUDE.md](../CLAUDE.md)'s note on that file.
