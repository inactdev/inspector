# Examiner runtime

The examiner runtime contains Inspector's trusted Linux agent executables for Docker's common AMD64 and ARM64 server platforms.

`build.sh` cross-compiles both static agents from `cmd/examiner-agent` with a pinned Go 1.22.12 toolchain, compresses them reproducibly, and prints their SHA-256 digests. It uses a digest-pinned builder container when the host toolchain differs. Run `build.sh check` to prove the committed executables match the reviewed source. After rebuilding, update the matching digest constants in `internal/examiner/runtime.go`. The `examiner_agent` build tag leaves the host-only container launcher and embedded artifacts out of the agent executable.
