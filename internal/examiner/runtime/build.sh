#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname "$0")/../../.." && pwd)
exec docker build -t inspector-examiner-agent:local -f "$root/internal/examiner/runtime/Dockerfile" "$root"
