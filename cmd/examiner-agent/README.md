# Examiner agent

The examiner agent is the trusted program compiled into Inspector's local examiner runtime image.

It runs only as the image entrypoint. The public `inspector` command never exposes it as a host subcommand. It reads the sealed inputs, asks the model to propose scenarios and assessments, records app attempts, and writes a record-derived verdict.
