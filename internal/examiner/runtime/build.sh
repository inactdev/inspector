#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname "$0")/../../.." && pwd)
output="$root/internal/examiner/runtime"
builder_image='cimg/go@sha256:a3b66b5f01291de5d4ddcaaf7916f1eabdb3c990da628f766a6d28f978a6928e'
builder_go_version='go1.22.12'

mode=${1:-build}
case "$mode" in
  build|check) ;;
  --local-build|--local-check) ;;
  *)
    echo "usage: $0 [build|check]" >&2
    exit 64
    ;;
esac

case "$mode" in
  --local-build|--local-check)
    if [ "$(go env GOVERSION)" != "$builder_go_version" ]; then
      echo "examiner agent requires $builder_go_version, found $(go env GOVERSION)" >&2
      exit 1
    fi
    ;;
  *)
    if [ "$(go env GOVERSION)" != "$builder_go_version" ]; then
      command -v docker >/dev/null 2>&1 || {
        echo "building the examiner agent requires $builder_go_version or Docker" >&2
        exit 1
      }
      docker_endpoint=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
      case "$docker_endpoint" in
        unix://*|npipe://*) ;;
        *)
          echo "building the examiner agent refuses remote Docker endpoint $docker_endpoint" >&2
          exit 1
          ;;
      esac
      docker_user="$(id -u):$(id -g)"
      case "$(docker info --format '{{json .SecurityOptions}}')" in
        *name=rootless*) docker_user=0:0 ;;
      esac
      docker run --rm --userns host --user "$docker_user" \
        --env HOME=/tmp --env GOCACHE=/tmp/go-build \
        --mount "type=bind,src=$root,dst=/workspace" \
        --workdir /workspace \
        "$builder_image" \
        internal/examiner/runtime/build.sh "--local-$mode"
      exit
    fi
    mode="--local-$mode"
    ;;
esac

build_dir=$(mktemp -d "$root/.examiner-agent-build.XXXXXX")
trap 'rm -rf "$build_dir"' EXIT HUP INT TERM

for arch in amd64 arm64; do
  binary="$build_dir/examiner-agent-linux-$arch"
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
  if [ "$mode" = "--local-check" ]; then
    gzip -dc "$output/examiner-agent-linux-$arch.gz" > "$build_dir/committed-$arch"
    if ! cmp -s "$binary" "$build_dir/committed-$arch"; then
      echo "bundled examiner agent for linux/$arch does not match its reviewed source" >&2
      exit 1
    fi
  else
    gzip -9 -n -c "$binary" > "$output/examiner-agent-linux-$arch.gz"
  fi
done

if [ "$mode" = "--local-check" ]; then
  echo "bundled examiner agents match their reviewed source"
elif command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$output"/*.gz
else
  shasum -a 256 "$output"/*.gz
fi
