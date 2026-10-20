# Syncing the gate's two files to consumer repositories

`inspector-gate` is **two** files, and a repository that runs it keeps its own
copy of both: `.github/workflows/inspector-gate.yml` and
`.github/scripts/inspector-gate.sh`. The workflow deliberately never checks the
repository out, so it reads the script from its own base branch through the
GitHub API - which means a consumer carrying only one of the two files fails
closed on every run, saying it could not load its own definition. Nothing kept
those copies in step; a fix to the gate landed here and silently did not reach
anyone running it.

`.github/workflows/gate-sync.yml` plus `.github/scripts/gate-sync.sh` are that
missing half, deliberately the same shape as the Fabrica consumer-contract sync
(`fabrica-contract-check.sh`'s opposite number on Fabrica's side): a push to
`main` that changes either gate file opens or updates one pull request, on one
fixed branch, on every repository named in the committed consumer list.

## The decisions worth remembering

**Why the API and not a clone.** The sync builds its commit entirely out of
GitHub Git Database calls - blob, tree, commit, ref - instead of cloning each
consumer, writing the files, and pushing. A clone would put a write-scoped
token into a git configuration next to somebody else's repository content, for
no gain: the thing being written is two files at two known paths. The API route
hands git nothing and executes nothing from the consumer. The files go up
base64-encoded as blobs rather than as tree-entry `content`, so "verbatim" does
not quietly mean "verbatim if it is valid UTF-8".

**Why the sync may check out, when the gate may not.** `gate-sync.yml` runs
`actions/checkout`, which `inspector-gate.yml` must never do. The difference is
the trigger, not the preference: `gate-sync` runs on `push` to `main`, so the
code it checks out is already merged and already past the gate, while
`inspector-gate` runs on `pull_request_target` against code that is not. Do not
read one as permission for the other.

**Why the branch is built on its own tip, never force-updated.** The commit's
parent is the sync branch's current tip when that branch exists, and the
consumer's default-branch tip only when it does not. So the ref update is
always a fast-forward, `force` is explicitly `false`, and nothing a reviewer
pushed to the branch is ever destroyed - a hand edit is replaced by the next
sync's commit on top of it, visibly, in the pull request's own history. A
rebase-and-force variant was considered and rejected for that reason. The cost
is that a long-lived sync branch can drift behind the consumer's default
branch; the pull request shows it, and merging resolves it.

**Why one failing consumer does not stop the others.** Each consumer is
attempted, its failure recorded with the reason, and the run fails at the end
naming every one that failed (an owner ruling). One repository's revoked access
must not leave the rest of them carrying a stale gate.

**Why the consumer list fails closed as a whole.** A malformed line, a
duplicate, or the source repository naming itself aborts the run before any
consumer is touched. The list is the one input that decides *where* a
write-scoped token gets pointed; acting on half of an ambiguous one is worse
than refusing all of it. It lives at `.github/gate-consumers.txt`, inside the
`.github/**` floor that `inspector-gate.sh`'s `ALWAYS_PROTECTED` covers, so
changing it needs the owner's eyes like any other gate file.

**Idempotence is load-bearing, not polish.** Before writing anything the sync
compares each file's base64 against the consumer's copy - on the default branch
first (already adopted, nothing to do), then on the sync branch (already
carried, leave the commit alone and only keep the pull request's text current).
Re-running after fixing one consumer's access therefore cannot churn the
healthy ones, which is the normal way this script gets run a second time.

**What the test can and cannot prove.** `gate-sync_test.sh` unit tests the pure
functions and reads `.github/workflows/gate-sync.yml` as text to prove its
facts: the trigger set, the path filter (which must equal the synced files plus
the consumer list - otherwise adding a consumer would never wake the sync), the
secret name, minimal permissions, and that every environment variable the
script requires is actually supplied by the workflow. The GitHub API
orchestration in `main()` is only exercisable inside Actions, the same honest
limit `inspector-gate.sh` and `fabrica-contract-check.sh` record for
themselves.
