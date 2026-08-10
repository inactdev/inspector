#!/usr/bin/env bash
# The single source of truth for the external tools script/check's
# preflight requires - sourced by script/check itself and by the drift
# test in inspector-gate_test.sh that proves this list is actually
# enough. Duplicating it in two places is exactly the kind of drift that
# broke the preflight the first time: "tr" was a real dependency of
# inspector-gate.sh that the hand-maintained list simply didn't name.
#
# Most of this list is proven complete by that drift test. The four
# exceptions are mktemp, ln, sed and rm: they are the drift test's own
# scaffolding, so it cannot prove them - see the drift block's comment in
# inspector-gate_test.sh. They are listed here anyway because script/check
# genuinely cannot run to completion without them.
REQUIRED_TOOLS="go gofmt bash jq base64 awk tr dirname mktemp ln sed rm"
