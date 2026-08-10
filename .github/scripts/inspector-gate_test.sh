#!/usr/bin/env bash
# Unit tests for inspector-gate.sh's pure decision functions
# (status_verdict, find_protected_match, listing_count_check,
# protected_paths_from_response). These take plain strings in and print
# plain strings out - no network, no GitHub API, no environment variables -
# so they can run anywhere, including here. The last one takes a raw API
# response as a string, so the decisions made on top of a response are
# tested here even though fetching one is not.
#
# What this deliberately does NOT cover: the GitHub API orchestration in
# main() - making the gh api calls, paginating them, and which ref each
# one is read at. That is real coverage this project doesn't have, not
# faked - it can only be exercised by an actual pull request going through
# Actions, since it depends on GitHub's API responses and the
# pull_request_target trust boundary. Testing it here would mean mocking
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

# --- protected_paths_from_response ---
#
# Fixtures are shaped like a real `gh api --include` response: status
# line, headers, blank line, then the Contents API's JSON body with the
# file base64'd into .content.

contents_response() {
  local status="$1" body="$2"
  printf 'HTTP/2.0 %s\r\nContent-Type: application/json; charset=utf-8\r\n\r\n%s\n' "$status" "$body"
}

inspector_json_response() {
  local file_content="$1"
  contents_response "200 OK" \
    "$(jq -nc --arg c "$(printf '%s' "$file_content" | base64 | tr -d '\n')" '{content: $c, encoding: "base64"}')"
}

assert_fails_closed() {
  local desc="$1" raw="$2" expected_substring="$3"
  local out rc=0
  out=$(protected_paths_from_response "$raw") || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "FAIL: $desc - returned success instead of failing closed (patterns: \"$out\")"
    failures=$((failures + 1))
    return
  fi
  case "$out" in
  *"$expected_substring"*) echo "ok: $desc" ;;
  *)
    echo "FAIL: $desc - failed closed, but the reason did not mention \"$expected_substring\""
    echo "  actual: $out"
    failures=$((failures + 1))
    ;;
  esac
}

assert_patterns() {
  local desc="$1" raw="$2" expected="$3"
  local out rc=0
  out=$(protected_paths_from_response "$raw") || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "FAIL: $desc - failed closed instead of succeeding: $out"
    failures=$((failures + 1))
    return
  fi
  assert_eq "$desc" "$expected" "$out"
}

assert_patterns "a 404 is the one safe absence: no project paths, no error" \
  "$(contents_response "404 Not Found" '{"message":"Not Found"}')" \
  ""

assert_fails_closed "a rate limit fails closed rather than reading as absent" \
  "$(contents_response "403 Forbidden" '{"message":"API rate limit exceeded"}')" \
  "HTTP 403"

assert_fails_closed "a server error fails closed rather than reading as absent" \
  "$(contents_response "502 Bad Gateway" '{"message":"Server Error"}')" \
  "HTTP 502"

assert_fails_closed "no response at all (a network failure) fails closed" \
  "" \
  "no HTTP response at all"

assert_fails_closed "a reply that is not an HTTP response fails closed" \
  "gh: could not resolve host" \
  "did not come back as an HTTP response"

assert_fails_closed "empty content (the API's 1MB inline limit) fails closed" \
  "$(contents_response "200 OK" '{"content":"","encoding":"none"}')" \
  "empty content"

assert_fails_closed "content that is not valid JSON fails closed" \
  "$(inspector_json_response 'check: script/check')" \
  "not valid JSON"

assert_fails_closed "valid JSON that is not an object fails closed, naming the type" \
  "$(inspector_json_response '["script/check"]')" \
  "its top level is a JSON array, not an object"

assert_fails_closed "a bare JSON null fails closed as a non-object, not as invalid JSON" \
  "$(inspector_json_response 'null')" \
  "its top level is a JSON null, not an object"

assert_fails_closed "protectedPaths present but not an array fails closed" \
  "$(inspector_json_response '{"protectedPaths": "script/check"}')" \
  "not an array"

assert_fails_closed "a protectedPaths entry that is not a string fails closed" \
  "$(inspector_json_response '{"protectedPaths": ["ok.sh", 7]}')" \
  "not a string"

assert_patterns "a config with no protectedPaths adds nothing" \
  "$(inspector_json_response '{"timeoutSeconds": 600}')" \
  ""

assert_patterns "check is never read - a bare-path check command adds nothing on its own" \
  "$(inspector_json_response '{"check": "script/check"}')" \
  ""

assert_patterns "check is never read even when it is not a string - only protectedPaths is validated" \
  "$(inspector_json_response '{"check": ["script/check"]}')" \
  ""

assert_patterns "protectedPaths entries come back regardless of what check contains" \
  "$(inspector_json_response '{"check": "script/check", "protectedPaths": ["script/check", "ci/**"]}')" \
  "$(printf 'script/check\nci/**')"

echo
if [ "$failures" -ne 0 ]; then
  echo "$failures assertion(s) failed."
  exit 1
fi
echo "all assertions passed."
