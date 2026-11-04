# Named build secrets for the check image build

Issue #86. Built 2026-11-04. Amends
`notes/20261025-project-image-from-committed-dockerfile.md` (the image the
check runs in is built from the project's own committed Dockerfile) and
`notes/20261103-the-repo-named-build-context.md` (that build also gets the
repository under check as a named context). Both describe a build that reads
only files; this adds the one thing a build may need that is in no file.

## The plan, and the alternative that was rejected

Chosen, safest first: `.inspector.json` gains `buildSecrets`, a list of
**environment variable names**, and `container.BuildProjectImage` turns each
into `docker build --secret id=NAME,env=NAME`. Docker reads the value out of
inspector's own environment itself and exposes it only to a `RUN` step that
mounts it; inspector never holds the value, never puts it on a command line,
and never writes it anywhere. Every name is resolved **before** the Dockerfile
is read, so a name that is unset or empty refuses the run with nothing built
and no docker command run at all.

Rejected: `--build-arg NAME=$NAME`, which is how most projects pass a token to
a build today and is one line shorter here. A build argument's value is
recorded in the image's own metadata, so `docker history` on the check image
would print the credential, and anyone who can read the image - the whole
point of a shared local tag - can read the secret. BuildKit's secret mounts
exist because build arguments leak; this is the case they were built for.

Also rejected: reading each value into a Go variable and passing it to docker
on its own file descriptor or in a temporary file. Either one puts the
credential somewhere inspector owns, which means a crash, a `%+v` on a struct,
or a leftover temporary file can expose it. `env=NAME` keeps the value in the
one place it already was.

## What changed

`.inspector.json`:

```json
{
  "check": "bundle exec rspec",
  "buildSecrets": ["BUNDLE_ENTERPRISE__CONTRIBSYS__COM"]
}
```

`Dockerfile.inspector`:

```dockerfile
FROM ruby:3.3
COPY --from=repo Gemfile Gemfile.lock /deps/
RUN --mount=type=secret,id=BUNDLE_ENTERPRISE__CONTRIBSYS__COM \
    cd /deps && \
    BUNDLE_ENTERPRISE__CONTRIBSYS__COM="$(cat /run/secrets/BUNDLE_ENTERPRISE__CONTRIBSYS__COM)" \
    bundle install
```

and the build inspector runs becomes

    docker build -f <Dockerfile.inspector> -t inspector-check:<hash> \
      --build-context repo=<repo root> \
      --secret id=NAME,env=NAME <main context>

one `--secret` per declared name, in the order the config lists them, with
repeats collapsed (see below).

## Why it is names only, in both directions

The value never reaches inspector at all. `env=NAME` tells docker which
variable of its own environment to read, so the credential travels from the
host environment into BuildKit and into the mounting `RUN` step without
passing through any string inspector formats, logs, reports or posts. What
inspector does with the value is exactly one thing: ask whether it is there,
and throw it away. `resolveBuildSecrets` reads it with `os.LookupEnv` and
keeps only the name - there is deliberately no field anywhere holding a
secret's value, so no future `%+v`, report marshal or error message can print
one.

That is also why a secret is not a `--build-arg`, and why
`ProjectImage.SecretNames` holds names: the run's console line says which
secrets the build was handed, because a build whose inputs are invisible is
not reviewable, and the names are not the secret.

## Unset or empty is a refusal, before anything is built

A declared name that inspector's environment does not carry, or carries as
whitespace, refuses the run by that name. Two reasons it is not left to the
build:

- Docker does not fail on its own. A `--secret id=X,env=X` whose variable is
  unset still starts the build; the mounting `RUN` step then reads an empty
  file (`required=true` on the mount is what turns that into an error, and
  that is the project's Dockerfile's choice, not inspector's). So the failure
  a project would actually get is `bundle install` reporting a 401 somewhere
  inside a build log - a refusal naming the variable is the same information
  twenty minutes earlier.
- Empty counts as missing on purpose, the same ruling `gate-sync.sh` already
  makes for an Actions secret: a secret that does not exist in CI arrives as
  an empty variable rather than an unset one, so the two have to be one case.

The check happens inside `container.BuildProjectImage`, ahead of reading the
Dockerfile and resolving its bases, rather than in `LoadConfig` or in
`run.go`. One choke point, in the function that owns the build, is what makes
"never a build without it" a property of the code rather than of every
caller's discipline.

## A name is `[A-Za-z0-9_]+`, and anything else is refused by name

Not a guess at what a shell would accept and not an escape routine: the name
goes into a comma-separated docker flag value, so a name carrying a comma, an
equals sign or a space would change which flag docker reads. Refusing the
character class outright is the one form that cannot be smuggled through.

Repeats collapse to one flag, keeping first-mentioned order, rather than being
refused or passed twice. Twice in the list says the same thing once, and what
`docker build` does with two `--secret` flags carrying one id is not
documented - the project does not build a call on undocumented behavior.

## What is unchanged on purpose

When an image is rebuilt. A Dockerfile with `RUN --mount` already counts as
reading the build context in `parseDockerfile`, so a Dockerfile that mounts a
secret is rebuilt every run and docker's layer cache is what keeps that cheap.
The content-derived tag does not hash the secret names, and must not start
hashing their values - a tag is published in reports and console lines.

## The check container never sees it

Only the build does. The check container's environment is the handful of
variables `container.New` sets by name (`GIT_CONFIG_*`, `HOME`), so a
credential needed to install dependencies is not sitting in the environment of
the project's own test suite. Nothing had to be done for that - it is what the
container already did - but it is the question anyone reviewing this will ask.

## Where this lives, and the one place that builds

`internal/container/secret.go` holds the whole rule;
`container.BuildProjectImage` is the only `docker build` call site in the
repository, and `buildArgs` in `internal/container/image.go` now assembles
that command line in one testable place. `internal/inspector/run.go` also
calls `container.ValidateBuildSecrets` before `container.EnsureAvailable`, for
the same reason the Dockerfile is resolved there: a missing declaration is
worth saying on a machine with no Docker at all. Those two calls are the same
function, not two copies of the rule.

The examiner's own runtime image (`runtime/Dockerfile`) takes no secrets and
must not: it is inspector's trusted image, built locally, and the examiner
forwards only an API key *name* into its sealed container
(`internal/examiner/container.go`) - the same names-only shape this follows.

## The exit code this deviates on

The issue asks for exit `64` for the unset-secret refusal. It is exit `2`,
with the deviation stated in the pull request: `64` is reserved for what is
not a verdict attempt at all - `--help`, an unrecognized flag, bad usage - and
README's own exit-code list puts "missing configuration" under `2`, which is
also what `cmd/inspector/testdata/fabrica-consumer-contract.json` pins as the
refusal code Fabrica reads. A declared secret that is not in the environment
is the same shape as a missing `Dockerfile.inspector` or a retired config key,
and all of those refuse with `2`.
