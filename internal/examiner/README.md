# Examiner

The examiner is Inspector's independent HTTP outcome judge.

It receives request text, a feature map, an always-true list, names of worker-changed files, and pre-task versions of changed tests. It never mounts the judged repository or reads worker-written source, test content, or diffs. Changed test names are signals: a pre-task test lets the model propose the scenario it protected, then the examiner drives that scenario in the running app.

The model proposes capabilities, app requests, and assessments. The machinery records which response was successfully delivered for each capability and derives the final outcome from that record. A capability without a successful observation cannot be confirmed. The record keeps incomplete examination separate from confirmed bugs.

The agent is compiled from reviewed Inspector source into the local image defined by `runtime/Dockerfile`. `runtime/build.sh` is the one build command. The examination itself never builds or pulls the runtime image.

The container runs under whatever user namespace the daemon gives it rather than opting out of one, so a `userns-remap` host keeps its isolation. Because such a daemon runs it as a different unprivileged host user, the agent has a second answer-and-exit mode beside `--runtime-fingerprint`: `--host-identity` prints the host `uid:gid` it resolves for itself through `/proc/self/uid_map`. The host side runs that probe once, after the fingerprint check has confirmed the image, and grants exactly that one uid the least access it needs to the staged inputs and the verdict it fills in - never a wider mode. See `host_access.go` and `notes/20261025-examiner-under-userns-remap.md`.
