#!/usr/bin/env bash
# The single source of truth for the external tools script/check's
# preflight requires - sourced by script/check itself and by the drift
# test in inspector-gate_test.sh that proves this list is actually
# enough. Duplicating it in two places is exactly the kind of drift that
# broke the preflight the first time: "tr" was a real dependency of
# inspector-gate.sh that the hand-maintained list simply didn't name.
REQUIRED_TOOLS="go gofmt bash jq base64 awk tr dirname"
