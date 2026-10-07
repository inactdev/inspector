# Builds the examiner runtime image used by internal/examiner.RuntimeImage.
# Build locally before examining an app:
#   docker build -t inspector-examiner-agent:local -f internal/examiner/runtime/Dockerfile .
#
# The host Inspector binary may be macOS or Windows and cannot execute in a
# Linux container. This image compiles the same reviewed agent source for the
# Docker server's platform instead. Nothing is published and `inspector
# examine` never builds or pulls it.
#
# This manifest-list digest resolves to Go 1.26.8 on Alpine 3.24.1, verified on
# the real machine when pinned. It stays multi-architecture for amd64 and arm64.
FROM golang@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/examiner-agent ./cmd/examiner-agent
COPY internal/container ./internal/container
COPY internal/examiner ./internal/examiner
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/examiner-agent ./cmd/examiner-agent

# This manifest-list digest resolves to Alpine 3.20.10, verified on the real
# machine when pinned. Keep this general-purpose Linux base: a future browser
# driver belongs in this image rather than creating a second runtime shape.
FROM alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
COPY --from=build /out/examiner-agent /usr/local/bin/examiner-agent
ENTRYPOINT ["/usr/local/bin/examiner-agent"]
