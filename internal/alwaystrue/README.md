# Always-true list (check 3)

This package owns the project's regression list and the runner that drives it.

The list is one file in the judged project - `.inspector-always.json` by
default - kept under that project's protected paths. It is the same artifact
the examiner reads as `--always-true`: one name, one file, one check.

Each line carries both halves. A `promise` is the plain-language statement a
person reads. A `falsify` is the console command, driver script, or HTTP
exchange that would disprove it. A line with no falsifier is reported as
**not-runnable** and is never reported as passing; a line that declares an
empty falsifier is refused when the list is read, because a falsifier that
checks nothing is the silent weakening the list exists to prevent.

`confirmed` records whether a person has accepted the line. It is reported and
counted and deliberately does not change the verdict - a broken promise nobody
accepted yet is still a broken promise.

Check 3 brings the app up itself, from the image the list names, with the
fixture data the list declares, waits for the declared readiness response, and
removes both afterwards. It never builds or pulls. It mounts **no repository**,
which is the whole of how read-only is guaranteed: there is no handle on the
commit to be read-only about, the same boundary the examiner has. The
consequence to know before changing the format: a driver script is named by its
path inside the runner image, never by a path in the project.

HTTP falsifiers are performed by Inspector itself against the ephemeral
loopback port Docker published for the app. Command and script falsifiers run
in their own single-use container joined with `--network container:<app>`, so
they share the app container's network stack and reach it at `APP_URL` -
`127.0.0.1` at the port the app actually listens on.

One aggregate verdict, never fail-fast. Every line is attempted. A broken
promise posts `failure` under the `inspector-always` status and names all of
them; an app that would not start or a line that could not be run posts
`error` instead and says which. See `notes/20261025-check-3-the-regression-list.md`,
README's "Always-true list" section, and issue #24.
