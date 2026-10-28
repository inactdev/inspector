# A timeout that fires before the container exists still has to stop the check

Issue #66. On a cold CI runner `docker run` can take seconds to get as far
as creating the container. A check whose deadline falls inside that window
used to be unstoppable: `Cancel` fired, `docker kill <name>` answered "No
such container", `killContainer` mapped that to `os.ErrProcessDone` (its
only honest reading when a `--rm` container may have just auto-removed
itself), and nothing else happened. Docker then created the container and
ran the check command to completion. `CheckResult.KillFailed` recorded the
refused kill, but recording is not stopping - the check kept running
against the repo past its deadline, which is the one thing the timeout
exists to prevent. `run 40706626397` is where it showed up, as a flaking
`TestRunCheck_TimeoutKillsChildProcesses`.

## What the kill path does now

`container.Cmd.kill` resolves the container id from the `--cidfile` Docker
writes at creation - the same file `Started` already reads - and waits for
it to appear when it is not there yet:

1. **Id already written** (the ordinary case): unchanged, `docker kill`.
2. **Container created but not started**: `docker kill` answers "is not
   running". That container is seconds away from being started by the
   `docker run` client, so it is removed by force instead. `docker rm
   --force` is the one stop that works in both states, so a container that
   starts between the two calls is still taken down.
3. **No id yet**: poll the cidfile until one appears, then (1)/(2). The
   wait is bounded by whatever is left of the run's own deadline plus 30
   seconds and is cut short as soon as the `docker run` client itself is
   gone - a creation that failed outright never writes a cidfile and never
   will. Never indefinite.
4. **Still no id after that budget**: kill the `docker run` client (it is
   the only handle left, and killing it keeps it from starting a container
   the daemon may still be creating), then `docker rm --force` whatever id
   has appeared since, by this run's unique container name if none has.
   `KillFailed` is set only if the container is then *confirmed* still
   running by `docker inspect`; a removal that worked, or a container that
   was never there, is not a failed kill.

Every outcome past (1) is reported in words, live on stderr and in the
report (`Cmd.StopNote`), because "the check timed out and was stopped" and
"the check timed out and inspector had to kill Docker's own client to stop
it" are different facts about the same run. The verdict is `TimedOut`
either way - issue #66 changes only whether the process is actually
stopped.

`Cmd.Wait`'s post-wait `docker rm --force` (added with the examiner) stays:
it is the last catch for a container the daemon created after step 4's
removal, and it clears `KillFailed` when the container is confirmed gone by
then.

The cost is bounded latency: a run whose deadline lands in the creation
window now returns up to the grace period (plus `WaitDelay`) later than its
deadline, rather than returning promptly and leaving a check running.
Blocking inside `Cancel` is safe by construction - `os/exec` starts its
`WaitDelay` timer only after `Cancel` returns (`watchCtx`), so the delay
window cannot open under us while we wait.

## The alternative that was rejected

Kill the `docker run` client immediately whenever the container is not
there yet, and skip the wait. It is simpler and faster, and it is wrong in
the direction that matters: the create request is already with the daemon,
so killing the client is a detach, not a stop - the daemon can finish
creating and starting a container nobody is attached to any more. Waiting
for the id means the thing that gets killed is the container itself.
Killing the client is kept only as the last resort in step 4, where no id
ever appeared, and even there it is followed by a removal and a running
check rather than trusted on its own.

A second alternative, polling `docker inspect` for the container by name
instead of watching the cidfile, was rejected as a second source of truth
for "does this container exist" when the cidfile is already the one
`Started` is built on - and it costs a `docker` process per poll.
