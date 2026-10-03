#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/../../.." && pwd)
output="$root/internal/examiner/runtime"

for arch in amd64 arm64; do
  binary="$output/examiner-agent-linux-$arch"
  (
    cd "$root"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build \
      -tags examiner_agent \
      -trimpath \
      -buildvcs=false \
      -ldflags='-s -w -buildid=' \
      -o "$binary" \
      ./cmd/examiner-agent
  )
  gzip -9 -n -f "$binary"
done

if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$output"/*.gz
else
  shasum -a 256 "$output"/*.gz
fi
