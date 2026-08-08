#!/usr/bin/env bash
# inspector-gate's decision logic (SPEC.md sections 7-8, issue #4). Small
# and dumb on purpose: two independent questions, both must pass.
#
#   1. Does the pull request's head commit carry a green commit status
#      under STATUS_CONTEXT? No status at all is red, same as a failing
#      one - SPEC.md section 7: "No result at all is red, the same as a
#      failing one."
#   2. Did the pull request touch a protected path? Any touch is red, full
#      stop - SPEC.md section 8: "There is no declared-changes path that
#      passes."
#
# SAFETY-CRITICAL, read before editing this file:
#
# The workflow that calls this script runs on `pull_request_target`, so
# GitHub loads the WORKFLOW's definition from the base branch, never from
# the pull request. This script is the same story: the workflow never
# checks out the pull request, so the copy of THIS file that actually runs
# is always the base branch's, never the branch under test. A pull request
# editing this file cannot change how it is judged - and since this file
# lives under .github/**, which ALWAYS_PROTECTED below covers, such an
# edit also fails question 2, every time.
#
# This script must never check out or execute anything from the pull
# request. It only asks the GitHub API which files changed and what status
# is posted, and compares strings. If you are tempted to add
# `actions/checkout` to the workflow to make this script's job easier:
# don't. That is exactly the vulnerability class `pull_request_target` is
# known for (elevated permissions plus untrusted code), and SPEC.md section
# 8 rules it out explicitly.
#
# This file is sourced by two callers:
#   - the workflow step, which calls main() to do the real GitHub API work
#   - inspector-gate_test.sh, which sources this file and calls the pure
#     functions below directly, with fixture data, never touching the
#     network
# Only the pure functions (status_verdict, find_protected_match,
# listing_count_check) are unit tested. main()'s API orchestration can only
# be exercised inside Actions - see inspector-gate_test.sh's header for why
# that is an honest limitation rather than a gap.
set -euo pipefail

# ALWAYS_PROTECTED protects the gate's own definition regardless of what a
# project's .inspector.json adds under protectedPaths: the workflow files,
# this script (and any sibling under .github/scripts), and .inspector.json
# itself, which could otherwise edit its own protectedPaths list away.
ALWAYS_PROTECTED='.github/**
.inspector.json'

# status_verdict prints two tab-separated fields for one context's status
# state: "green" or "red", then a reason (empty for green). Absent (empty
# state) and any non-"success" state both come back red - nobody reading
# silence is meant to mistake it for approval.
status_verdict() {
  local state="$1" context="$2"
  if [ -z "$state" ]; then
    printf 'red\tno "%s" status has been posted to this commit at all\n' "$context"
    return
  fi
  if [ "$state" = "success" ]; then
    printf 'green\t\n'
    return
  fi
  printf 'red\tthe "%s" status on this commit is "%s", not "success"\n' "$context" "$state"
}

# find_protected_match scans touched (one "file<TAB>previous_file" line per
# changed entry, previous_file empty unless it's a rename) against patterns
# (one glob pattern per line, `[[ ]]`-style). It prints the first matching
# path and returns 0, or returns 1 if nothing matches. `read -r` per line
# (not word-splitting) so a pattern or filename containing a space is
# matched whole rather than silently split into two things that can never
# match a real path.
find_protected_match() {
  local touched="$1" patterns="$2"
  local pattern file previous_file candidate
  while IFS= read -r pattern; do
    [ -n "$pattern" ] || continue
    while IFS=$'\t' read -r file previous_file; do
      [ -n "$file" ] || continue
      for candidate in "$file" "$previous_file"; do
        [ -n "$candidate" ] || continue
        if [[ "$candidate" == $pattern ]]; then
          printf '%s\n' "$candidate"
          return 0
        fi
      done
    done <<< "$touched"
  done <<< "$patterns"
  return 1
}

# listing_count_check compares how many files the API actually listed
# against the pull request's own reported changed_files count. They can
# disagree for two different reasons, both of which must fail closed
# rather than pass on a list that might be incomplete:
#   - the API caps a single pull request's file listing at 3000 entries,
#     silently, no matter how many pages are requested (prints "cap")
#   - a new commit landed mid-run, so the live listing no longer matches
#     the count this run started with (prints "race")
# Equal counts print "ok".
listing_count_check() {
  local listed="$1" changed="$2"
  if [ "$listed" = "$changed" ]; then
    echo "ok"
    return
  fi
  if [ "$listed" -eq 3000 ] 2>/dev/null && [ "$changed" -gt 3000 ] 2>/dev/null; then
    echo "cap"
    return
  fi
  echo "race"
}

# main does the real GitHub API work: fetch the head commit's status,
# fetch the pull request's changed-file list, fetch the base branch's
# .inspector.json for project-specific protected paths, then apply the
# pure functions above. Only exercisable inside Actions - see this file's
# header.
main() {
  : "${GH_TOKEN:?}" "${REPO:?}" "${PR_NUMBER:?}" "${HEAD_SHA:?}" "${BASE_SHA:?}" "${CHANGED_FILES:?}" "${STATUS_CONTEXT:?}"

  local failed=0

  # Question 1: does HEAD_SHA carry a green STATUS_CONTEXT status?
  #
  # `gh api` has no way to hand a jq filter an external variable (no
  # --arg), so STATUS_CONTEXT is built into the filter string with jq's
  # own $ENV lookup instead of shell interpolation - it comes from this
  # workflow's own env block, not the pull request, so it's trusted, but
  # this avoids relying on that being true forever.
  local state
  state=$(STATUS_CONTEXT="$STATUS_CONTEXT" gh api "repos/$REPO/commits/$HEAD_SHA/status" \
    --jq '[.statuses[] | select(.context == env.STATUS_CONTEXT)] | first | .state // ""')
  local verdict reason
  IFS=$'\t' read -r verdict reason <<< "$(status_verdict "$state" "$STATUS_CONTEXT")"
  if [ "$verdict" = "red" ]; then
    echo "::error::inspector status check failed - $reason. Absent and failing are treated the same: run inspector locally and let it push a green \"$STATUS_CONTEXT\" status to this exact commit before merging."
    failed=1
  else
    echo "inspector status: green."
  fi

  # Question 2: did the pull request touch a protected path?
  #
  # A pure rename reports as ONE entry: the new filename, plus
  # previous_filename. Emitting only .filename would let a pull request
  # rename a protected file away undetected; previous_filename closes that.
  #
  # One tab-separated line per entry, deliberately: the line count is then
  # exactly the number of files GitHub listed, which listing_count_check
  # compares against the pull request's own changed_files count.
  local touched listed
  touched=$(gh api "repos/$REPO/pulls/$PR_NUMBER/files?per_page=100" --paginate \
    --jq '.[] | [.filename, (.previous_filename // empty)] | @tsv')
  if [ -z "$touched" ]; then
    listed=0
  else
    listed=$(printf '%s\n' "$touched" | wc -l | tr -d '[:space:]')
  fi

  local count_check
  count_check=$(listing_count_check "$listed" "$CHANGED_FILES")
  case "$count_check" in
  cap)
    echo "::error::Could not read this pull request's complete changed-file list: GitHub listed $listed file(s), but the pull request itself reports $CHANGED_FILES changed. The API caps that listing at 3000 files, so this pull request cannot be checked against the protected-path list. Failing closed rather than passing unchecked - split the pull request into smaller ones, or have the Client review it and deliberately override this check."
    exit 1
    ;;
  race)
    echo "::error::This pull request's changed-file count changed while this check was running (GitHub listed $listed file(s) just now, but this run started when the pull request reported $CHANGED_FILES) - most likely a new commit was pushed mid-run, not a listing problem. Failing closed rather than checking a stale count; a fresh run on the latest push resolves this on its own, or the Client can review and deliberately override this check."
    exit 1
    ;;
  esac

  # Project-specific protected paths come from the BASE branch's copy of
  # .inspector.json (ref=$BASE_SHA), never the pull request's - reading it
  # any other way would let a pull request add or remove its own
  # protected-path entries. Missing or unparsable is not an error: a
  # project with no .inspector.json, or one that hasn't adopted
  # protectedPaths yet, still gets the ALWAYS_PROTECTED floor.
  local project_paths=""
  local config_json
  if config_json=$(gh api "repos/$REPO/contents/.inspector.json?ref=$BASE_SHA" --jq '.content' 2>/dev/null | tr -d '\n' | base64 -d 2>/dev/null); then
    project_paths=$(printf '%s' "$config_json" | jq -r '.protectedPaths // [] | .[]' 2>/dev/null || true)
  fi

  local patterns="$ALWAYS_PROTECTED"
  if [ -n "$project_paths" ]; then
    patterns="$patterns
$project_paths"
  fi

  local match
  if match=$(find_protected_match "$touched" "$patterns"); then
    echo "::error::This pull request touches \"$match\", a protected file. Any touch to a protected path is red, full stop - there is no declared-changes path that passes (SPEC.md section 8). Only the Client may review this by hand and merge it using his own bypass."
    failed=1
  else
    echo "protected files: none touched."
  fi

  if [ "$failed" -ne 0 ]; then
    exit 1
  fi
  echo "inspector-gate: green."
}

# Allow sourcing (for tests) without running main.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
