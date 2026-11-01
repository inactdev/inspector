#!/usr/bin/env bash
# The single source of truth for the external tools script/check's
# preflight requires - sourced by script/check itself and by the drift
# test in inspector-gate_test.sh that proves this list is actually
# enough. Duplicating it in two places is exactly the kind of drift that
# broke the preflight the first time: "tr" was a real dependency of
# inspector-gate.sh that the hand-maintained list simply didn't name.
#
# "cat" and "wc" arrived the same way, and are the reason to trust this
# mechanism rather than a reviewer's eye: both are inspector-gate.sh's own
# dependencies - gh_api_call reads its captured stderr with cat, and main()
# counts the changed-file listing with wc - and neither was named here for
# as long as nothing exercised main(). Issue #21's main() scenarios do, so
# the drift test found them on their first run.
#
# Most of this list is proven complete by that drift test. The two
# exceptions are ln and sed: they run only in the drift test's own outer
# scaffolding (and sed only when it has a failure to print), so the
# restricted run can never exercise them - see the drift block's comment in
# inspector-gate_test.sh. They are listed here anyway because script/check
# genuinely cannot run to completion without them.
REQUIRED_TOOLS="go gofmt bash jq base64 awk tr cat wc dirname mktemp ln sed rm"
