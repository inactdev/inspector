#!/usr/bin/env bash
# Unit tests for fabrica-contract-check.sh's pure functions
# (decode_contents_response, api_failure_reason). These take plain
# strings in and print plain strings out - no network, no GitHub API -
# so they can run anywhere, including here.
#
# What this deliberately does NOT cover: the GitHub API orchestration in
# main() - resolving Fabrica's default branch, fetching its contract at
# that ref, diffing it against the local copy. That can only be
# exercised by a real Actions run; see this file's and
# fabrica-contract-check.sh's headers, and inspector-gate_test.sh's for
# the same reasoning applied to that gate.
#
# Run directly: .github/scripts/fabrica-contract-check_test.sh
set -euo pipefail
cd "$(dirname "$0")"

# shellcheck source=fabrica-contract-check.sh
source ./fabrica-contract-check.sh

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

# --- api_failure_reason ---

assert_eq "a single-line gh error comes through as-is" \
  "gh: HTTP 403: API rate limit exceeded" \
  "$(api_failure_reason "gh: HTTP 403: API rate limit exceeded" 1)"

assert_eq "a multi-line gh error is joined onto one line" \
  "gh: HTTP 403; API rate limit exceeded; try again later" \
  "$(api_failure_reason "$(printf 'gh: HTTP 403\nAPI rate limit exceeded\ntry again later')" 1)"

assert_eq "a silent failure still names the exit status instead of trailing off" \
  "gh exited 7 without printing a reason" \
  "$(api_failure_reason "" 7)"

# --- decode_contents_response ---
#
# Fixtures are shaped like a real `gh api --include` response: status
# line, headers, blank line, then the Contents API's JSON body with the
# file base64'd into .content.

contents_response() {
  local status="$1" body="$2"
  printf 'HTTP/2.0 %s\r\nContent-Type: application/json; charset=utf-8\r\n\r\n%s\n' "$status" "$body"
}

file_response() {
  local file_content="$1"
  contents_response "200 OK" \
    "$(jq -nc --arg c "$(printf '%s' "$file_content" | base64 | tr -d '\n')" '{content: $c, encoding: "base64"}')"
}

assert_fails_closed() {
  local desc="$1" raw="$2" expected_substring="$3" gh_error="${4:-}"
  local out rc=0
  out=$(decode_contents_response "$raw" "$gh_error") || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "FAIL: $desc - returned success instead of failing closed (content: \"$out\")"
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

assert_eq "a 200 response decodes to the file's real content" \
  '{"flags":[{"name":"branch"}]}' \
  "$(decode_contents_response "$(file_response '{"flags":[{"name":"branch"}]}')")"

# Unlike inspector-gate.sh's protected_paths_from_response, a 404 here is
# NOT a safe absence - there is no legitimate state where Fabrica's
# contract is missing and inspector's copy can still be trusted, so it
# must fail closed and be named like any other non-200.
assert_fails_closed "a 404 fails closed - it is not a safe absence for a required contract file" \
  "$(contents_response "404 Not Found" '{"message":"Not Found"}')" \
  "HTTP 404"

assert_fails_closed "a rate limit fails closed, naming the status" \
  "$(contents_response "403 Forbidden" '{"message":"API rate limit exceeded"}')" \
  "HTTP 403"

assert_fails_closed "a server error fails closed, naming the status" \
  "$(contents_response "502 Bad Gateway" '{"message":"Server Error"}')" \
  "HTTP 502"

assert_fails_closed "no response at all (a network failure) fails closed" \
  "" \
  "no HTTP response at all"

assert_fails_closed "no response at all names gh's own reason when the caller has it" \
  "" \
  "no HTTP response at all: dial tcp: lookup api.github.com: no such host" \
  "dial tcp: lookup api.github.com: no such host"

assert_fails_closed "a reply that is not an HTTP response fails closed" \
  "gh: could not resolve host" \
  "did not come back as an HTTP response"

assert_fails_closed "empty content (the API's 1MB inline limit) fails closed" \
  "$(contents_response "200 OK" '{"content":"","encoding":"none"}')" \
  "empty content"

assert_fails_closed "a 200 whose body is not JSON says so" \
  "$(contents_response "200 OK" '{"content": truncated mid-resp')" \
  "not readable JSON"

assert_fails_closed "content that is not valid base64 says so" \
  "$(contents_response "200 OK" '{"content":"!!! not base64 !!!","encoding":"base64"}')" \
  "not valid base64"

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "all fabrica-contract-check tests passed"
