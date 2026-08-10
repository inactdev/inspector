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
# The workflow runs on two events, and both answer the same two questions
# about the same pull request:
#   - pull_request_target, which carries the pull request itself
#   - status, which carries only a commit sha, and which exists so the
#     gate re-answers question 1 the moment inspector's green status
#     lands - see main_status_event() for how it stays inside the same
#     trust boundary
#
# This file is sourced by two callers:
#   - the workflow step, which calls main() or main_status_event() to do
#     the real GitHub API work
#   - inspector-gate_test.sh, which sources this file and calls the pure
#     functions below directly, with fixture data, never touching the
#     network
# Only the pure functions (status_verdict, find_protected_match,
# listing_count_check, protected_paths_from_response,
# pull_request_for_status) are unit tested. The API orchestration in
# main()/main_status_event() can only be exercised inside Actions - see
# inspector-gate_test.sh's header for why that is an honest limitation
# rather than a gap.
set -euo pipefail

# ALWAYS_PROTECTED protects the gate's own definition regardless of what a
# project's .inspector.json adds under protectedPaths: the workflow files,
# this script (and any sibling under .github/scripts), and .inspector.json
# itself, which could otherwise edit its own protectedPaths list away.
# The project's own check command is protected too, but that path is not
# knowable here - it is read out of .inspector.json by
# protected_paths_from_response below.
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

# protected_paths_from_response turns the GitHub Contents API's raw
# response for the BASE branch's .inspector.json into that project's own
# protected patterns, one per line: its protectedPaths entries, plus its
# check command when, and only when, that value names a bare
# repo-relative path. A check command is precisely the thing a worker
# under pressure edits to make a failing check stop failing (SPEC.md
# section 8), so a repo that copies this gate gets a `"check":
# "script/check"` protected without listing it twice.
#
# But `check` holds a command line, not necessarily a path: `npm test &&
# npm run lint` names no single file, and adding it as a pattern would
# only produce something that can never match a filename while reading,
# in the log, like a protection that exists. Anything with whitespace, a
# shell metacharacter, a leading `/`, or a `..` segment is therefore left
# out entirely - such a project must list the files it wants protected
# under protectedPaths itself. A leading `./` is dropped rather than
# disqualifying, since `./check.sh` and `check.sh` are the same file and
# only the second form is what the files API reports.
#
# It fails CLOSED. On anything it cannot read with certainty it prints one
# reason line and returns 1, and the caller must treat that as a hard
# error rather than quietly falling back to the ALWAYS_PROTECTED floor: a
# gate that protects less than it claims is worse than one that refuses.
# The single safe absence is HTTP 404 - the file genuinely is not there,
# and a project that has not adopted .inspector.json still gets the floor.
#
# The argument is the complete `gh api --include` response (status line,
# headers, blank line, body), or "" when the call produced no response at
# all.
protected_paths_from_response() {
  local raw="$1"
  local proto http_status rest body content kind

  if [ -z "$raw" ]; then
    echo "the request for it produced no HTTP response at all (a network failure, or gh could not reach GitHub)"
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
  content=$(jq -r '.content // ""' <<< "$body" 2>/dev/null | tr -d '\n' | base64 -d 2>/dev/null) || content=""
  if [ -z "$content" ]; then
    echo "it came back with empty content (the Contents API only inlines files up to 1MB, so a larger one reads as empty here)"
    return 1
  fi
  if ! jq empty >/dev/null 2>&1 <<< "$content"; then
    echo "it is not valid JSON, so the paths it protects cannot be read"
    return 1
  fi
  kind=$(jq -r 'type' <<< "$content")
  if [ "$kind" != "object" ]; then
    echo "its top level is a JSON $kind, not an object, so it has no protected paths to read"
    return 1
  fi
  kind=$(jq -r '.protectedPaths | type' <<< "$content")
  if [ "$kind" != "null" ] && [ "$kind" != "array" ]; then
    echo "its \"protectedPaths\" is a JSON $kind, not an array of paths"
    return 1
  fi
  if ! jq -e 'all((.protectedPaths // [])[]; type == "string")' >/dev/null <<< "$content"; then
    echo "one of its \"protectedPaths\" entries is not a string"
    return 1
  fi
  kind=$(jq -r '.check | type' <<< "$content")
  if [ "$kind" != "null" ] && [ "$kind" != "string" ]; then
    echo "its \"check\" is a JSON $kind, not a string"
    return 1
  fi

  local check
  check=$(jq -r '.check // ""' <<< "$content")
  check="${check#./}"
  if [[ "$check" =~ ^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$ ]] && [[ ! "$check" =~ (^|/)\.\.?(/|$) ]]; then
    printf '%s\n' "$check"
  fi
  jq -r '(.protectedPaths // [])[]' <<< "$content"
}

# pull_request_for_status picks the pull request a `status` event concerns
# out of GitHub's "list pull requests associated with a commit" response.
# A status event carries no pull request context at all - just the commit
# a status was posted to - so the gate has to look this up before it can
# ask its two questions about anything.
#
# It prints "number<TAB>head_sha<TAB>base_sha<TAB>changed_files" for the
# first OPEN pull request whose head commit is exactly this sha, and
# returns 0. No associated pull requests, only closed ones, or an open one
# whose head has since moved elsewhere all return 1 and print nothing:
# that commit heads no open pull request, so there is genuinely nothing to
# gate. A response that is not even a list returns 2 - that is uncertainty,
# not absence, and the caller fails closed on it.
#
# changed_files normally comes back empty, because this endpoint returns
# the short form of a pull request, which omits it; main_status_event
# fetches the full object when it does.
pull_request_for_status() {
  local json="$1" sha="$2"
  local match
  jq -e 'type == "array"' >/dev/null 2>&1 <<< "$json" || return 2
  match=$(SHA="$sha" jq -r '
    [ .[]
      | select(.state == "open")
      | select(.head.sha == env.SHA)
    ]
    | first
    | if . == null then empty
      else [(.number | tostring), .head.sha, .base.sha, (.changed_files // "" | tostring)] | @tsv
      end
  ' <<< "$json" 2>/dev/null) || return 2
  [ -n "$match" ] || return 1
  printf '%s\n' "$match"
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
  # This reads the per-commit statuses LIST endpoint, not the combined
  # commits/{sha}/status one. The combined endpoint's statuses array is
  # paginated at 30 and supports no pagination of its own, so on a commit
  # carrying more than 30 contexts a green "inspector" can silently fall
  # off the page and be reported as "no status posted at all" - a false
  # red that no re-run fixes. The list endpoint pages properly and returns
  # every status ever posted to the commit, newest first and not deduped
  # by context, so the first match is the current one.
  #
  # `gh api` has no way to hand a jq filter an external variable (no
  # --arg), so STATUS_CONTEXT is built into the filter string with jq's
  # own $ENV lookup instead of shell interpolation - it comes from this
  # workflow's own env block, not the pull request, so it's trusted, but
  # this avoids relying on that being true forever.
  local states state
  states=$(STATUS_CONTEXT="$STATUS_CONTEXT" gh api "repos/$REPO/statuses/$HEAD_SHA?per_page=100" --paginate \
    --jq '.[] | select(.context == env.STATUS_CONTEXT) | .state')
  state="${states%%$'\n'*}"
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
  # protected-path entries. --include so the HTTP status line comes back
  # with the body: only a 404 means "this project simply has no
  # .inspector.json", and every other way this read can go wrong fails
  # closed rather than shrinking the protected list to the floor without
  # saying so. See protected_paths_from_response.
  local config_response project_paths
  config_response=$(gh api --include "repos/$REPO/contents/.inspector.json?ref=$BASE_SHA" 2>/dev/null) || true
  if ! project_paths=$(protected_paths_from_response "$config_response"); then
    echo "::error::Could not read the base branch's .inspector.json, so this pull request cannot be checked against the protected paths this project actually declares: $project_paths. Failing closed rather than checking against a shorter list - re-run this check, or have the Client review it and deliberately override it."
    exit 1
  fi

  local patterns="$ALWAYS_PROTECTED"
  if [ -n "$project_paths" ]; then
    patterns="$patterns
$project_paths"
  fi

  # Echo what is actually in effect, so a run's own log shows the list it
  # judged against rather than leaving that to be inferred.
  local pattern
  echo "protected patterns in effect:"
  while IFS= read -r pattern; do
    [ -n "$pattern" ] || continue
    echo "  $pattern"
  done <<< "$patterns"

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

# main_status_event is the entry point for GitHub's `status` event. It
# exists because of an ordering fact: a commit status can only be posted
# to a commit that already exists on the remote, so inspector's green
# status necessarily lands seconds AFTER the push whose `synchronize` run
# already answered question 1 with "no status has been posted at all".
# Without a re-run on the status arriving, that first honest red would
# stand until a human re-ran the workflow by hand.
#
# A status event carries no pull request context, only a commit sha, so
# this looks up which open pull request (if any) that commit heads - one
# read-only API call, the same trust model as the changed-file list and
# .inspector.json. It still checks out nothing and executes nothing from
# the pull request.
#
# Trust root: a status event has no ref of its own worth trusting, so the
# workflow bootstraps this file from the repository's DEFAULT branch, a
# fixed root no pull request controls. Once the pull request is known,
# this re-fetches this same file from that pull request's real BASE ref
# and re-sources it, so the copy that decides the outcome is the one
# pull_request_target would have used - and that happens before any
# decision is made.
#
# A commit that heads no open pull request is a real, honest success:
# there is nothing to gate. Never a skip.
main_status_event() {
  : "${GH_TOKEN:?}" "${REPO:?}" "${STATUS_SHA:?}" "${STATUS_CONTEXT:?}"

  local pulls fields="" rc=0
  pulls=$(gh api "repos/$REPO/commits/$STATUS_SHA/pulls?per_page=100" --paginate)
  fields=$(pull_request_for_status "$pulls" "$STATUS_SHA") || rc=$?
  if [ "$rc" -eq 2 ]; then
    echo "::error::GitHub's list of pull requests associated with commit $STATUS_SHA came back in a shape this gate could not read. Failing closed rather than assuming that commit heads no pull request."
    exit 1
  fi
  if [ "$rc" -ne 0 ]; then
    echo "no open pull request has $STATUS_SHA as its head commit - there is nothing here to gate."
    return 0
  fi

  local number head_sha base_sha changed_files
  IFS=$'\t' read -r number head_sha base_sha changed_files <<< "$fields"
  if [ -z "$changed_files" ]; then
    changed_files=$(gh api "repos/$REPO/pulls/$number" --jq '.changed_files')
  fi
  echo "commit $STATUS_SHA is the head of pull request #$number - re-checking it."

  local base_script
  base_script=$(gh api "repos/$REPO/contents/.github/scripts/inspector-gate.sh?ref=$base_sha" --jq '.content' | tr -d '\n' | base64 -d)
  source <(printf '%s' "$base_script")

  PR_NUMBER="$number" HEAD_SHA="$head_sha" BASE_SHA="$base_sha" CHANGED_FILES="$changed_files" main
}

# Allow sourcing (for tests) without running main.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
