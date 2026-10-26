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
# Question 2 has exactly one narrow exception, issue #44's stopgap: a
# protected path that is also one of the project's own declared testPaths
# passes when the head commit carries a green TESTS_STATUS_CONTEXT status,
# which is inspector's record that every changed test file in it was judged
# to still prove what the request asked for. Nothing else clears a protected
# path, and ALWAYS_PROTECTED below is never clearable this way - a project
# must not be able to hand .github/** or .inspector.json to a model's
# judgment. The gate itself stays AI-free: it reads a status string, exactly
# as it does for question 1, and runs no model and no project code.
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
# Only the pure functions (status_verdict, state_for_context,
# matches_any_pattern, each_touched_candidate, protected_match_kind,
# classify_protected_matches, listing_count_check, gh_api_failure_reason,
# config_paths_from_response and its two wrappers) are unit tested. The
# API orchestration in main() can only be exercised inside Actions - see
# inspector-gate_test.sh's header for why that is an honest limitation
# rather than a gap.
set -euo pipefail

# ALWAYS_PROTECTED protects the gate's own definition regardless of what a
# project's .inspector.json adds under protectedPaths: the workflow files,
# this script (and any sibling under .github/scripts), .inspector.json
# itself, which could otherwise edit its own protectedPaths list away, and
# Dockerfile.inspector, which is the environment the check command is run
# in. That last one is here because inspector#47 moved it: the image used
# to be named by .inspector.json's "image" field, already covered by the
# line below, and leaving its replacement unprotected would have quietly
# narrowed what this list covers. Swapping the box a check runs in is
# another way to make a failing check stop failing, which is exactly what
# SPEC.md section 8 is about.
#
# A project's own check command is NOT derived automatically from
# .inspector.json's "check" field and added here - that was tried and
# reverted. Telling a path from a command line by looking at the string
# does not hold up: "script/check" is a path, "pytest" is not, and
# "check.sh" could be either, so every heuristic that tries to split them
# keeps producing exactly the kind of false claim this project has already
# had to correct once. A project must list its own check command's file(s)
# under protectedPaths itself - see the "inspector-gate" section of
# README.md. Making that hard to forget is inspector#5's installer's job:
# it knows the concrete value at install time and can write it where a
# human sees and confirms it. Guessing at runtime is the wrong layer.
ALWAYS_PROTECTED='.github/**
.inspector.json
Dockerfile.inspector'

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

# state_for_context picks one context's current state out of the statuses
# list endpoint's "context<TAB>state" lines. That endpoint returns every
# status ever posted to the commit, newest first and never deduped by
# context, so the FIRST line naming a context is its current state. Prints
# nothing when that context was never posted to this commit, which
# status_verdict reads as red - the same as a failing one.
state_for_context() {
  local listing="$1" context="$2"
  local line_context line_state
  while IFS=$'\t' read -r line_context line_state; do
    [ -n "$line_context" ] || continue
    if [ "$line_context" = "$context" ]; then
      printf '%s\n' "$line_state"
      return 0
    fi
  done <<< "$listing"
  return 0
}

# matches_any_pattern reports whether one candidate path matches any of
# patterns (one glob pattern per line, `[[ ]]`-style). `read -r` per line
# (not word-splitting) so a pattern containing a space is matched whole
# rather than silently split into two things that can never match a real
# path. An empty pattern list matches nothing.
matches_any_pattern() {
  local candidate="$1" patterns="$2"
  local pattern
  [ -n "$candidate" ] || return 1
  while IFS= read -r pattern; do
    [ -n "$pattern" ] || continue
    # $pattern is deliberately unquoted: it is a glob, and quoting it
    # would turn every protected pattern into an exact string match,
    # so ".github/**" would stop protecting anything under .github/.
    # shellcheck disable=SC2053
    if [[ "$candidate" == $pattern ]]; then
      return 0
    fi
  done <<< "$patterns"
  return 1
}

# each_touched_candidate prints every path one changed-file listing names -
# one per line, the new name and, for a rename, the previous one too. A pure
# rename reports as ONE entry whose previous_filename is the only place the
# old (possibly protected) path appears, which is why both are emitted.
each_touched_candidate() {
  local touched="$1"
  local file previous_file candidate
  while IFS=$'\t' read -r file previous_file; do
    [ -n "$file" ] || continue
    for candidate in "$file" "$previous_file"; do
      [ -n "$candidate" ] || continue
      printf '%s\n' "$candidate"
    done
  done <<< "$touched"
}

# protected_match_kind says how one candidate path is protected, which
# decides what - if anything - can clear it:
#
#   blocked  red, full stop. Nothing clears it.
#   test     red unless the head commit carries a green TESTS_STATUS_CONTEXT
#            status (issue #44): the path is protected AND the project's own
#            testPaths claim it as one of its tests.
#   (none)   not protected; returns 1 and prints nothing.
#
# always is checked FIRST and is never clearable, even if a project's own
# testPaths would also match it - otherwise a project could declare
# ".github/**" a test path and hand the gate's own definition to a model's
# judgment. A test pattern only ever downgrades a path that the project's
# own protectedPaths made red in the first place; a test file nobody
# protected was never blocked and needs nothing to pass.
protected_match_kind() {
  local candidate="$1" always="$2" project="$3" tests="$4"
  if matches_any_pattern "$candidate" "$always"; then
    echo "blocked"
    return 0
  fi
  if matches_any_pattern "$candidate" "$project"; then
    if matches_any_pattern "$candidate" "$tests"; then
      echo "test"
    else
      echo "blocked"
    fi
    return 0
  fi
  return 1
}

# classify_protected_matches prints one "kind<TAB>path" line for every
# protected path a changed-file listing touches - every one, not just the
# first, so the log names each file that has to be dealt with rather than
# one at a time across re-runs. Returns 0 whether or not anything matched;
# the caller decides on the lines.
classify_protected_matches() {
  local touched="$1" always="$2" project="$3" tests="$4"
  local candidate kind
  while IFS= read -r candidate; do
    [ -n "$candidate" ] || continue
    if kind=$(protected_match_kind "$candidate" "$always" "$project" "$tests"); then
      printf '%s\t%s\n' "$kind" "$candidate"
    fi
  done <<< "$(each_touched_candidate "$touched")"
  return 0
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

# gh_api_failure_reason turns a failed `gh api` call's captured stderr into
# the one-line reason an ::error:: annotation can carry. Annotations are
# single-line - a raw newline ends the annotation and drops everything
# after it - so gh's lines are joined with "; " instead. An empty capture
# still names the exit status, so the message never trails off after a
# colon with nothing behind it.
gh_api_failure_reason() {
  local stderr_text="$1" exit_status="$2"
  local joined
  joined=$(printf '%s\n' "$stderr_text" | tr -d '\r' |
    awk 'NF { gsub(/^[[:space:]]+|[[:space:]]+$/, ""); out = (out == "" ? $0 : out "; " $0) } END { print out }')
  if [ -z "$joined" ]; then
    printf 'gh exited %s without printing a reason\n' "$exit_status"
    return
  fi
  printf '%s\n' "$joined"
}

# config_paths_from_response turns the GitHub Contents API's raw response
# for the BASE branch tip's .inspector.json into the string entries of one
# named array field in it, one per line. Two fields are read through it:
# "protectedPaths" (what may not be touched) and "testPaths" (which of the
# project's own files are tests, issue #44). It does NOT derive anything
# from the "check" field - see ALWAYS_PROTECTED's comment for why that was
# tried and reverted; a project must list its own check command's file(s)
# under protectedPaths explicitly.
#
# It fails CLOSED. On anything it cannot read with certainty it prints one
# reason line and returns 1, and the caller must treat that as a hard
# error rather than quietly falling back to the ALWAYS_PROTECTED floor: a
# gate that protects less than it claims is worse than one that refuses.
# The single safe absence is HTTP 404 - the file genuinely is not there,
# and a project that has not adopted .inspector.json still gets the floor.
# An absent field inside a readable file is the other safe absence, and
# means the same thing it meant before that field existed: no entries.
#
# The first argument is the complete `gh api --include` response (status
# line, headers, blank line, body), or "" when the call produced no
# response at all. The second is the field name to read. The third is
# optional: gh's own one-line explanation for that call, already run
# through gh_api_failure_reason. It is the only thing that can say WHY
# there was no response - DNS, an unreachable GitHub, a gh crash all look
# identical from an empty string - so when the caller has it, that reason
# is named instead of guessing between them.
config_paths_from_response() {
  local raw="$1" field="$2" gh_error="${3:-}"
  local proto http_status rest body encoded content kind

  if [ -z "$raw" ]; then
    if [ -n "$gh_error" ]; then
      echo "the request for it produced no HTTP response at all: $gh_error"
    else
      echo "the request for it produced no HTTP response at all (a network failure, or gh could not reach GitHub)"
    fi
    return 1
  fi
  read -r proto http_status rest <<< "$raw" || true
  http_status="${http_status%$'\r'}"
  case "$proto" in
  HTTP/*) ;;
  *)
    echo "the reply did not come back as an HTTP response (its first line was \"$proto\")"
    return 1
    ;;
  esac
  case "$http_status" in
  404) return 0 ;;
  200) ;;
  *)
    echo "the GitHub Contents API returned HTTP $http_status for it - only a 404, the file genuinely not existing, is a safe absence"
    return 1
    ;;
  esac

  body=$(awk 'in_body { print } /^\r?$/ { in_body = 1 }' <<< "$raw")

  # Each step names its own failure. Reading the body, un-base64ing it, and
  # the file genuinely being empty are three different things that used to
  # collapse into one message blaming the API's 1MB inline-content limit -
  # which was a guess, and wrong for two of the three. This function's whole
  # contract is naming the reason it refused, so it has to name the real one.
  if ! encoded=$(jq -r '.content // ""' <<< "$body" 2>/dev/null); then
    echo "its response body was not readable JSON, so the file's content could not be found in it"
    return 1
  fi
  encoded=$(tr -d '\n' <<< "$encoded")
  if [ -z "$encoded" ]; then
    echo "it came back with empty content (the Contents API only inlines files up to 1MB, so a larger one reads as empty here)"
    return 1
  fi
  if ! content=$(base64 -d <<< "$encoded" 2>/dev/null); then
    echo "its content was not valid base64, so the file could not be decoded"
    return 1
  fi
  if [ -z "$content" ]; then
    echo "it decoded to an empty file, so the paths it protects cannot be read"
    return 1
  fi
  if ! jq empty >/dev/null 2>&1 <<< "$content"; then
    echo "it is not valid JSON, so the paths it protects cannot be read"
    return 1
  fi
  kind=$(jq -r 'type' <<< "$content")
  if [ "$kind" != "object" ]; then
    echo "its top level is a JSON $kind, not an object, so it has no \"$field\" to read"
    return 1
  fi
  # --arg rather than interpolating $field into the filter: the field name
  # is this script's own literal either way, but a filter built by string
  # concatenation is one edit away from being built from a response.
  kind=$(jq -r --arg f "$field" '.[$f] | type' <<< "$content")
  if [ "$kind" != "null" ] && [ "$kind" != "array" ]; then
    echo "its \"$field\" is a JSON $kind, not an array of paths"
    return 1
  fi
  if ! jq -e --arg f "$field" 'all((.[$f] // [])[]; type == "string")' >/dev/null <<< "$content"; then
    echo "one of its \"$field\" entries is not a string"
    return 1
  fi

  jq -r --arg f "$field" '(.[$f] // [])[]' <<< "$content"
}

# protected_paths_from_response reads the "protectedPaths" entries: the
# paths this project declares no pull request may touch.
protected_paths_from_response() {
  config_paths_from_response "$1" "protectedPaths" "${2:-}"
}

# test_paths_from_response reads the "testPaths" entries: which of this
# project's own files are tests, and therefore which protected path a green
# TESTS_STATUS_CONTEXT status is allowed to clear (issue #44). A project
# that declares none gets no entries and no exception - exactly the gate's
# behavior before this field existed.
test_paths_from_response() {
  config_paths_from_response "$1" "testPaths" "${2:-}"
}

# gh_api_call runs one `gh api` call the way all three of main()'s calls
# need it run: gh's stderr captured to a file rather than left to reach the
# log raw, and turned into one annotation-safe line by
# gh_api_failure_reason. It puts the response in GH_API_OUT (still set on
# failure - the Contents API's 404 body is a real answer, not noise) and
# the reason in GH_API_ERROR, then returns gh's own exit status, so each
# call site keeps its own wording and its own exit-or-continue decision.
#
# Part of main()'s orchestration, and untested for the same reason the
# rest of it is: it only does anything when there is a real `gh` and a real
# GitHub behind it. See this file's header.
GH_API_OUT=""
GH_API_ERROR=""
gh_api_call() {
  local stderr_file rc=0
  GH_API_OUT=""
  GH_API_ERROR=""
  stderr_file=$(mktemp) || {
    GH_API_ERROR="a temporary file to capture gh's stderr could not be created"
    return 1
  }
  GH_API_OUT=$(gh api "$@" 2>"$stderr_file") || rc=$?
  if [ "$rc" -ne 0 ]; then
    GH_API_ERROR=$(gh_api_failure_reason "$(cat "$stderr_file")" "$rc")
  fi
  rm -f "$stderr_file"
  return "$rc"
}

# main does the real GitHub API work: fetch the head commit's status,
# fetch the pull request's changed-file list, fetch the base branch tip's
# .inspector.json for project-specific protected paths, then apply the
# pure functions above. Only exercisable inside Actions - see this file's
# header.
main() {
  : "${GH_TOKEN:?}" "${REPO:?}" "${PR_NUMBER:?}" "${HEAD_SHA:?}" "${BASE_REF:?}" "${CHANGED_FILES:?}" "${STATUS_CONTEXT:?}" "${TESTS_STATUS_CONTEXT:?}"

  local failed=0

  # Question 1: does HEAD_SHA carry a green STATUS_CONTEXT status?
  #
  # This reads the per-commit statuses LIST endpoint, not the combined
  # commits/{sha}/status one. The combined endpoint's statuses array is
  # paginated at 30 and supports no pagination of its own, so on a commit
  # carrying more than 30 contexts a green "inspector" can silently fall
  # off the page and be reported as "no status posted at all" - a false
  # red that no re-run fixes. The list endpoint pages properly and returns
  # every status ever posted to the commit, newest first and not deduped
  # by context, so the first match is the current one.
  #
  # Every context's state comes back from this ONE call, as
  # "context<TAB>state" lines, and state_for_context picks the two this
  # gate asks about out of it. One call, not two: both questions are then
  # answered from the same snapshot of the commit's statuses, so a status
  # posted between two reads cannot produce a verdict that matches neither
  # moment. No jq filter needs a context name interpolated into it either.
  #
  # gh's own failure is caught and worded here rather than left to `set -e`.
  # An unguarded assignment aborts main() on the spot with nothing in the
  # log but gh's raw stderr, so a rate limit or a token-scope problem reads
  # exactly like a real red - and question 2 never runs to say otherwise.
  # Fail closed either way, but say which of the calls failed and why.
  local statuses state
  if ! gh_api_call "repos/$REPO/statuses/$HEAD_SHA?per_page=100" --paginate \
    --jq '.[] | [.context, .state] | @tsv'; then
    echo "::error::Could not read this commit's commit statuses from the GitHub API, so neither whether a green \"$STATUS_CONTEXT\" status exists nor whether its changed test files were judged is known: $GH_API_ERROR. Failing closed rather than guessing - this is a problem reaching GitHub, not a verdict on the code; re-run this check."
    exit 1
  fi
  statuses="$GH_API_OUT"
  state=$(state_for_context "$statuses" "$STATUS_CONTEXT")
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
  if ! gh_api_call "repos/$REPO/pulls/$PR_NUMBER/files?per_page=100" --paginate \
    --jq '.[] | [.filename, (.previous_filename // empty)] | @tsv'; then
    echo "::error::Could not read this pull request's changed-file list from the GitHub API, so it cannot be checked against the protected-path list at all: $GH_API_ERROR. Failing closed rather than passing unchecked - re-run this check."
    exit 1
  fi
  touched="$GH_API_OUT"
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

  # Project-specific protected paths come from the BASE BRANCH TIP's copy
  # of .inspector.json (ref=BASE_REF, the branch name, so the API resolves
  # it live), never the pull request's - reading it any other way would let
  # a pull request add or remove its own protected-path entries.
  #
  # The tip, deliberately, and not the pull request payload's base.sha: the
  # sha is a snapshot from when the pull request was opened or last pushed,
  # so adding an entry to protectedPaths on the base branch would not apply
  # to any pull request already open - tightening protection would silently
  # leave existing pull requests judged under the looser old list, which is
  # backwards for what a protection list is for. It also matches where
  # GitHub loads this workflow's own definition from, which is always the
  # base branch tip. The accepted cost is that a pull request's verdict can
  # change without the pull request changing: a new commit to the base
  # branch's .inspector.json can flip an already-open, untouched pull
  # request from pass to fail. That is the safe direction.
  #
  # BASE_REF is a branch NAME, so it is passed as a parameter (-X GET -f)
  # and never pasted into the query string. git allows "&", "#", "%" and
  # "+" in a ref name; interpolated raw, "release&hotfix" would send
  # ref=release and read a DIFFERENT branch's protected-path list, which is
  # a quiet fail-open in the one read this gate must get right. gh
  # percent-encodes a -f value, so no ref name can break out of it. Like
  # the rest of main(), that only runs against the real API - there is no
  # pure function here to unit test, see this file's header.
  #
  # --include so the HTTP status line comes back with the body: only a 404
  # means "this project simply has no .inspector.json", and every other way
  # this read can go wrong fails closed rather than shrinking the protected
  # list to the floor without saying so. See protected_paths_from_response.
  #
  # gh exits non-zero on any non-2xx, including the 404 that is the one
  # safe absence here, so the exit status alone decides nothing - the
  # response body does. But gh's stderr is captured like the other two
  # calls rather than discarded: when there is no response at all, it is
  # the only thing that can say which failure it was.
  # The same response answers both config questions - protectedPaths and
  # testPaths - so there is one read, and the two lists can never come from
  # two different versions of the file.
  local config_response project_paths test_patterns config_error
  gh_api_call -X GET --include "repos/$REPO/contents/.inspector.json" -f ref="$BASE_REF" || true
  config_response="$GH_API_OUT"
  config_error="$GH_API_ERROR"
  if ! project_paths=$(protected_paths_from_response "$config_response" "$config_error"); then
    echo "::error::Could not read the base branch's .inspector.json, so this pull request cannot be checked against the protected paths this project actually declares: $project_paths. Failing closed rather than checking against a shorter list - re-run this check, or have the Client review it and deliberately override it."
    exit 1
  fi
  if ! test_patterns=$(test_paths_from_response "$config_response" "$config_error"); then
    echo "::error::Could not read the base branch's .inspector.json, so which of this pull request's protected files are this project's own tests is unknown: $test_patterns. Failing closed rather than guessing - re-run this check, or have the Client review it and deliberately override it."
    exit 1
  fi

  local patterns="$ALWAYS_PROTECTED"
  if [ -n "$project_paths" ]; then
    patterns="$patterns
$project_paths"
  fi

  # Echo what is actually in effect, so a run's own log shows the lists it
  # judged against rather than leaving that to be inferred.
  local pattern
  echo "protected patterns in effect:"
  while IFS= read -r pattern; do
    [ -n "$pattern" ] || continue
    echo "  $pattern"
  done <<< "$patterns"
  if [ -z "$test_patterns" ]; then
    echo "test patterns in effect: none declared, so no protected path can be cleared by a \"$TESTS_STATUS_CONTEXT\" status."
  else
    echo "test patterns in effect (clearable by a green \"$TESTS_STATUS_CONTEXT\" status):"
    while IFS= read -r pattern; do
      [ -n "$pattern" ] || continue
      echo "  $pattern"
    done <<< "$test_patterns"
  fi

  # Every protected path this pull request touches, split by what can clear
  # it. A "blocked" one is red full stop; a "test" one waits on the
  # TESTS_STATUS_CONTEXT status read below.
  local classified blocked_paths="" test_paths="" kind path
  classified=$(classify_protected_matches "$touched" "$ALWAYS_PROTECTED" "$project_paths" "$test_patterns")
  while IFS=$'\t' read -r kind path; do
    [ -n "$kind" ] || continue
    case "$kind" in
    blocked) blocked_paths="$blocked_paths \"$path\"" ;;
    test) test_paths="$test_paths \"$path\"" ;;
    esac
  done <<< "$classified"

  if [ -n "$blocked_paths" ]; then
    echo "::error::This pull request touches${blocked_paths}, protected file(s). Any touch to a protected path is red, full stop - there is no declared-changes path that passes (SPEC.md section 8). Only the Client may review this by hand and merge it using his own bypass."
    failed=1
  fi

  # Issue #44's stopgap, and the only exception question 2 has: a protected
  # path the project itself declares a test passes when inspector has
  # recorded that every changed test in this commit still proves what the
  # request asked for. Absent or non-success is red, exactly as for question
  # 1 - this is the same status_verdict, reading the same statuses snapshot,
  # so silence is never mistaken for approval.
  if [ -n "$test_paths" ]; then
    local tests_state tests_verdict tests_reason
    tests_state=$(state_for_context "$statuses" "$TESTS_STATUS_CONTEXT")
    IFS=$'\t' read -r tests_verdict tests_reason <<< "$(status_verdict "$tests_state" "$TESTS_STATUS_CONTEXT")"
    if [ "$tests_verdict" = "red" ]; then
      echo "::error::This pull request changes${test_paths}, protected test file(s), and $tests_reason - so whether those changes still prove what the request asked for has not been established. Run inspector locally so it judges the changed tests and posts a green \"$TESTS_STATUS_CONTEXT\" status to this exact commit, or have the Client review it and deliberately override this check."
      failed=1
    else
      echo "protected test files cleared by a green \"$TESTS_STATUS_CONTEXT\" status:${test_paths}"
    fi
  fi

  if [ -z "$blocked_paths" ] && [ -z "$test_paths" ]; then
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
