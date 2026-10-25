# The CI check job and the images its tests pull

The repository's own `check` job went red on the guidebook change while every
other surface of the same tree was green. This note is what the run says, what
it does not say, and the change made because of it.

## What the run says

The job's only failing step is `script/check`, and the only annotation GitHub
exposes about that failure is `Process completed with exit code 1.` - the step
log itself is not readable without credentials, so **the individual failing test
cannot be named from here**. Everything below is what the run plus the rest of
the evidence narrows it down to, not a line number read off a log.

What is established:

- The same job is green on the default branch and on the preceding pull
  request, so the red is specific to this tree.
- The commit's own `inspector` status is green, and inspector only publishes
  green. That check runs in `cimg/go:1.22`: real Go 1.22, cgo, `-race`, and the
  three shell suites all passed on this exact tree.
- `gofmt`, `go vet`, `go build` and the whole test suite minus the
  container-gated tests pass locally, repeatedly, under shuffled order.
- Every test the two green surfaces do **not** cover is container-gated - 60 of
  them, in `internal/container`, `internal/inspector` and `cmd/inspector`. Both
  green surfaces lack a usable Docker daemon (inspector's own check container
  has no daemon inside it and no network), so those 60 tests skip there and run
  only on the CI runner.

So the failure is in the one surface only the runner exercises, and the whole
of that surface's dependence on anything outside the machine is two anonymous
image pulls: `alpine` and `buildpack-deps:bookworm-scm`. Both are unpinned,
unauthenticated reads of a public registry that rate-limits anonymous pulls per
source address - and CI runners share addresses with everything else on their
host network. A refused or half-finished pull is exactly a red that appears on
one run of one tree and not the next, with no defect in the commit.

The gate in front of those tests could not see it. `requireDocker` asks
`container.EnsureAvailable()` whether there is a reachable daemon, which is the
right question and only half of it: the tests also need the image they are
about to run, and a daemon that cannot obtain that image is no more usable for
them than no daemon at all. Without that half, a registry refusal arrives as an
ordinary error inside a test and becomes `t.Fatalf` - a red that blames the
commit for the registry.

## The change

Make the image part of what the existing gate means by a usable runtime:
`internal/dockertest.RequireImage` obtains each image once per test binary -
`docker image inspect` first, then a bounded, backed-off `docker pull` - and
skips with the registry's own words when it cannot be obtained at all. The
three gates call it; the tests that run the git image name that one too.

No assertion is weakened and no test is removed: whenever the image is
obtainable, all 60 tests run and assert exactly what they asserted before. Two
tests that deliberately name an unusable image are now also gated on the
default image they do not need; that over-skips them in the one case where the
registry is refusing anyway, which is cheaper than editing forty call sites to
be exact about it.

Obtaining the image up front also takes the pull out of the first test's own
timing budget. Several of these tests assert on 300ms and 500ms deadlines, and
the first one to touch a cold image currently pays a 170MB download inside
them.

## The alternative I rejected

**Pull the images in a CI step before `script/check`, or log in to the registry
with a secret.** It is one line of YAML, it is the standard advice, and it would
very likely have made this job green.

Rejected because it fixes one machine. `script/check` is the project's
definition of green wherever it runs - a factory worker, a laptop, a reviewer's
terminal - and all of them pull these images anonymously too. A workflow step
would leave every one of them failing a check for a registry's rate limit while
the runner alone was immune, and it would do it by editing a protected path for
a reason no reviewer can verify from the diff. The authenticated variant is
worse: it makes the suite depend on a credential the project does not have and
cannot hand to a contributor. Putting the tolerance where the other half of it
already lives costs a small test-only package and holds everywhere.

Also considered and rejected: serializing the suite (`-p 1`) to cut load, which
slows every run and does nothing about a refusal; and relaxing or deleting
container-gated assertions, which is the one thing that must not happen to make
a job green.

## Everywhere else with the same shape

Every other place a test or a run depends on something outside the machine,
and what it does about it:

- `requireInternet` (`internal/inspector/check_test.go`) - already skips on an
  unreachable outside world. This change is the same tolerance, one layer in.
- `ensureRuntimeImage` (`internal/examiner/container.go`) - already an error the
  examiner's tests turn into a skip, and it never pulls or builds by design.
- `.github/scripts/fabrica-contract-check.sh` - reads Fabrica's contract over
  the network and fails **closed** on every non-200. Same shape, deliberately
  the opposite policy, and left alone: there is no state where that file is
  unreadable and the local copy can still be trusted.
- The image in a project's own `.inspector.json` - inspector's is
  `cimg/go:1.22` - is pulled anonymously by `docker run` at check time, inside
  the check's timeout, in production rather than in tests. Not changed here:
  that is issue #47's committed-image requirement, and the fix is to stop
  naming a registry image at all rather than to tolerate the registry better.
- `alpine` and `buildpack-deps:bookworm-scm` in the tests are still unpinned
  tags. Pinning them would be reproducible but is a different change, and does
  nothing about a refusal.

## What this does not claim

It does not claim to have read the failing test's name. If the red was
something else in that same 60-test surface, this hardening will not have fixed
it - but it will have removed the one cause that can turn a correct commit red
through nothing the commit did, and the next run's output narrows what is left.

One residual, pre-existing and unchanged: `script/check` runs `go test` without
`-v`, so a skip is invisible there. That is already true of every one of these
60 tests on a machine with no daemon; this change extends the same deliberate
tolerance to a refused image rather than inventing a new silence.
