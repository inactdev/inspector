# Examiner

The examiner is Inspector's independent HTTP outcome judge.

It receives request text, a feature map, an always-true list, names of worker-changed files, and pre-task versions of changed tests. It never mounts the judged repository or reads worker-written source, test content, or diffs. Changed test names are signals: a pre-task test lets the model propose the scenario it protected, then the examiner drives that scenario in the running app.

The model proposes capabilities, app requests, and assessments. The machinery records which response was successfully delivered for each capability and derives the final outcome from that record. A capability without a successful observation cannot be confirmed. The record keeps incomplete examination separate from confirmed bugs.

The agent is compiled from reviewed Inspector source into the local image defined by `runtime/Dockerfile`. `runtime/build.sh` is the one build command. The examination itself never builds or pulls the runtime image.
