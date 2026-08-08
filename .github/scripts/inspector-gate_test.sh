#!/usr/bin/env bash
# Unit tests for inspector-gate.sh's pure decision functions
# (status_verdict, find_protected_match, listing_count_check). These take
# plain strings in and print plain strings out - no network, no GitHub API,
# no environment variables - so they can run anywhere, including here.
#
# What this deliberately does NOT cover: main()'s GitHub API orchestration
# (the gh api calls, pagination, and the base64/jq decoding of
# .inspector.json). That is real coverage this project doesn't have, not
# faked - it can only be exercised by an actual pull request going through
# Actions, since it depends on GitHub's API responses and the
# pull_request_target trust boundary (reading the base branch's copy of
# this script and of .inspector.json). Testing it here would mean mocking
# `gh api` well enough that the mock, not GitHub's behavior, is what the
# test actually proves.
#
# Run directly: .github/scripts/inspector-gate_test.sh
set -euo pipefail
cd "$(dirname "$0")"

# shellcheck source=inspector-gate.sh
source ./inspector-gate.sh

failures=0

assert_eq() {
  local desc="$1" expected="$2" actual="$3"
  if [ "$expected" != "$actual" ]; then
    echo "FAIL: $desc"
    echo "  expected: $expected"
    echo "  actual:   $actual"
    failures=$((failures + 1))
  else
    echo "ok: $desc"
  fi
}

# --- status_verdict ---

assert_eq "success state is green" \
  "$(printf 'green\t')" \
  "$(status_verdict "success" "inspector")"

assert_eq "empty state (absent status) is red" \
  "$(printf 'red\tno "inspector" status has been posted to this commit at all')" \
  "$(status_verdict "" "inspector")"

assert_eq "failure state is red, not conflated with absent" \
  "$(printf 'red\tthe "inspector" status on this commit is "failure", not "success"')" \
  "$(status_verdict "failure" "inspector")"

assert_eq "pending state is red" \
  "$(printf 'red\tthe "inspector" status on this commit is "pending", not "success"')" \
  "$(status_verdict "pending" "inspector")"

# --- find_protected_match ---

touched_clean=$'src/main.go\t\nREADME.md\t'
touched_dirty=$'src/main.go\t\n.inspector.json\t'
touched_workflow_edit=$'.github/workflows/inspector-gate.yml\t'
touched_rename=$'newname.txt\told-protected.txt'

if out=$(find_protected_match "$touched_clean" "$ALWAYS_PROTECTED"); then
  echo "FAIL: clean pull request should not match any protected path (matched \"$out\")"
  failures=$((failures + 1))
else
  echo "ok: clean pull request matches nothing"
fi

assert_eq "editing .inspector.json is caught" \
  ".inspector.json" \
  "$(find_protected_match "$touched_dirty" "$ALWAYS_PROTECTED")"

assert_eq "editing a workflow file is caught" \
  ".github/workflows/inspector-gate.yml" \
  "$(find_protected_match "$touched_workflow_edit" "$ALWAYS_PROTECTED")"

assert_eq "renaming a protected file away is caught via previous_filename" \
  "old-protected.txt" \
  "$(find_protected_match "$touched_rename" "$(printf 'old-protected.txt')")"

assert_eq "a project's own protectedPaths entry is honored" \
  "script/check" \
  "$(find_protected_match "$(printf 'script/check\t')" "$(printf 'script/check')")"

# --- listing_count_check ---

assert_eq "matching counts are ok" "ok" "$(listing_count_check 3 3)"
assert_eq "3000 listed against a larger declared count is the API's hard cap" \
  "cap" "$(listing_count_check 3000 3001)"
assert_eq "any other mismatch is a mid-run push race" \
  "race" "$(listing_count_check 5 6)"
assert_eq "a mismatch below 3000 is a race, not a cap, even at the boundary" \
  "race" "$(listing_count_check 2999 3000)"

echo
if [ "$failures" -ne 0 ]; then
  echo "$failures assertion(s) failed."
  exit 1
fi
echo "all assertions passed."
