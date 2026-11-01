#!/usr/bin/env bash
# Unit tests for inspector-gate.sh's pure decision functions
# (status_verdict, state_for_context, matches_any_pattern,
# each_touched_candidate, protected_match_kind,
# classify_protected_matches, listing_count_check, gh_api_failure_reason,
# and config_paths_from_response via both its wrappers). These take plain
# strings in and print
# plain strings out - no network, no GitHub API, no environment variables -
# so they can run anywhere, including here. The last one takes a raw API
# response as a string, so the decisions made on top of a response are
# tested here even though fetching one is not.
#
# The last two sections cover main() - the orchestration: the three GitHub
# API reads, in order, and what the gate does with each answer - against a
# stubbed `gh` serving fixtures, still off-network (issue #21). They also
# read .github/workflows/inspector-gate.yml as text for the facts main()
# depends on and cannot see for itself: which events wake the gate, which it
# must never be woken by, and that every environment variable main()
# requires is actually supplied to it.
#
# What no part of this file covers, and cannot: whether the real `gh` and
# the real GitHub behave the way those fixtures claim they do, and the
# pull_request_target trust boundary itself. Those are only exercised by an
# actual pull request going through Actions. The stub proves this gate's
# sequencing and its handling of each response; it cannot promise the
# response shapes it is handed are right.
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

# The same function answers the changed-test question (issue #44), so the
# wording follows the context it was asked about rather than being hardcoded
# to "inspector" - a worker reading it has to know WHICH status is missing.
assert_eq "an absent inspector-tests status is red, naming that context" \
  "$(printf 'red\tno "inspector-tests" status has been posted to this commit at all')" \
  "$(status_verdict "" "inspector-tests")"

assert_eq "a failing inspector-tests status is red, naming that context" \
  "$(printf 'red\tthe "inspector-tests" status on this commit is "failure", not "success"')" \
  "$(status_verdict "failure" "inspector-tests")"

# --- state_for_context ---
#
# The statuses list endpoint returns every status ever posted, newest first
# and never deduped by context, so the first line naming a context is its
# current state.

statuses_listing=$'inspector-tests\tsuccess\ninspector\tsuccess\ninspector\tfailure\nexaminer\terror'

assert_eq "a context's newest state is the one read" \
  "success" "$(state_for_context "$statuses_listing" "inspector")"

assert_eq "a second context is read independently of the first" \
  "success" "$(state_for_context "$statuses_listing" "inspector-tests")"

assert_eq "an unrelated context is not confused for a prefix match" \
  "" "$(state_for_context "$statuses_listing" "inspector-")"

assert_eq "a never-posted context reads as empty, which status_verdict calls red" \
  "" "$(state_for_context "$statuses_listing" "inspector-tests-extra")"

assert_eq "no statuses at all reads as empty" \
  "" "$(state_for_context "" "inspector")"

assert_eq "an older state does not shadow the newest one" \
  "$(printf 'green\t')" \
  "$(status_verdict "$(state_for_context "$statuses_listing" "inspector")" "inspector")"

# --- protected-path matching ---
#
# These five assertions predate issue #44, when one function
# (find_protected_match) answered "is any protected path touched" by
# printing the first match. main() now needs every match AND what can clear
# each one, so classify_protected_matches replaced it and the unused
# function was removed rather than left behind. The claims are unchanged -
# the same five pull requests, the same five protected paths found - and
# each now also pins the kind, which is what decides the verdict. An empty
# test-pattern list is passed here deliberately: with no testPaths declared,
# every protected match must come back "blocked", which is the gate's
# behavior before this exception existed.

touched_clean=$'src/main.go\t\nREADME.md\t'
touched_dirty=$'src/main.go\t\n.inspector.json\t'
touched_workflow_edit=$'.github/workflows/inspector-gate.yml\t'
touched_rename=$'newname.txt\told-protected.txt'

assert_eq "clean pull request matches nothing" \
  "" \
  "$(classify_protected_matches "$touched_clean" "$ALWAYS_PROTECTED" "" "")"

assert_eq "editing .inspector.json is caught" \
  "$(printf 'blocked\t.inspector.json')" \
  "$(classify_protected_matches "$touched_dirty" "$ALWAYS_PROTECTED" "" "")"

assert_eq "editing a workflow file is caught" \
  "$(printf 'blocked\t.github/workflows/inspector-gate.yml')" \
  "$(classify_protected_matches "$touched_workflow_edit" "$ALWAYS_PROTECTED" "" "")"

# inspector#47 moved the check environment out of .inspector.json's
# "image" field and into this file. It is protected for the same reason
# that field was: swapping the box a check runs in is another way to make
# a failing check stop failing.
assert_eq "editing Dockerfile.inspector is caught" \
  "$(printf 'blocked\tDockerfile.inspector')" \
  "$(classify_protected_matches "$(printf 'Dockerfile.inspector\t')" "$ALWAYS_PROTECTED" "" "")"

assert_eq "renaming a protected file away is caught via previous_filename" \
  "$(printf 'blocked\told-protected.txt')" \
  "$(classify_protected_matches "$touched_rename" "" "$(printf 'old-protected.txt')" "")"

assert_eq "a project's own protectedPaths entry is honored" \
  "$(printf 'blocked\tscript/check')" \
  "$(classify_protected_matches "$(printf 'script/check\t')" "" "$(printf 'script/check')" "")"

# --- each_touched_candidate ---

assert_eq "both halves of a rename entry are candidates, in order" \
  "$(printf 'newname.txt\nold-protected.txt')" \
  "$(each_touched_candidate "$touched_rename")"

assert_eq "an ordinary entry contributes only its filename" \
  "$(printf 'src/main.go\nREADME.md')" \
  "$(each_touched_candidate "$touched_clean")"

assert_eq "an empty listing contributes no candidates" \
  "" \
  "$(each_touched_candidate "")"

# --- matches_any_pattern ---

assert_matches() {
  local desc="$1" candidate="$2" patterns="$3" want="$4"
  local got=no
  if matches_any_pattern "$candidate" "$patterns"; then got=yes; fi
  assert_eq "$desc" "$want" "$got"
}

assert_matches "a glob matches across directory separators" \
  ".github/workflows/x.yml" "$ALWAYS_PROTECTED" "yes"
assert_matches "an unprotected path matches nothing" \
  "src/main.go" "$ALWAYS_PROTECTED" "no"
assert_matches "an empty pattern list matches nothing" \
  ".inspector.json" "" "no"
assert_matches "a blank line in the pattern list is not a match-anything pattern" \
  "src/main.go" "$(printf '\n\n')" "no"
assert_matches "a pattern containing a space is matched whole, not word-split" \
  "docs/my notes.md" "$(printf 'docs/my notes.md')" "yes"

# --- protected_match_kind / classify_protected_matches ---
#
# Issue #44's stopgap lives here: a protected path the project itself
# declares a test is "test" (clearable by a green inspector-tests status);
# everything else protected is "blocked" (nothing clears it).

project_paths_fixture=$(printf 'script/check\n*_test.go')
test_patterns_fixture=$(printf '*_test.go')

assert_kind() {
  local desc="$1" candidate="$2" want="$3"
  local got
  if ! got=$(protected_match_kind "$candidate" "$ALWAYS_PROTECTED" "$project_paths_fixture" "$test_patterns_fixture"); then
    got="(unprotected)"
  fi
  assert_eq "$desc" "$want" "$got"
}

assert_kind "a changed test file the project protects is clearable by the test status" \
  "internal/reviewer/tests_test.go" "test"
assert_kind "a protected non-test file is blocked outright" \
  "script/check" "blocked"
assert_kind "an unprotected file is not a match at all" \
  "internal/reviewer/tests.go" "(unprotected)"

# The floor outranks testPaths, always. Otherwise a project could declare
# ".github/**" a test path and hand the gate's own definition to a model's
# judgment - which is exactly what ALWAYS_PROTECTED exists to prevent.
assert_eq "ALWAYS_PROTECTED is never clearable, even when a project calls it a test path" \
  "blocked" \
  "$(protected_match_kind ".github/workflows/inspector-gate.yml" "$ALWAYS_PROTECTED" "$(printf '.github/**')" "$(printf '.github/**')")"

assert_eq ".inspector.json is never clearable by the test status either" \
  "blocked" \
  "$(protected_match_kind ".inspector.json" "$ALWAYS_PROTECTED" "$(printf '.inspector.json')" "$(printf '.inspector.json')")"

# A project that declares no testPaths gets no exception: its protected
# test files stay blocked, exactly as they were before this existed.
assert_eq "with no testPaths declared, a protected test file stays blocked" \
  "blocked" \
  "$(protected_match_kind "pkg/thing_test.go" "$ALWAYS_PROTECTED" "$project_paths_fixture" "")"

assert_eq "a test file nobody protected needs no clearing - it was never blocked" \
  "" \
  "$(protected_match_kind "pkg/thing_test.go" "$ALWAYS_PROTECTED" "" "$test_patterns_fixture" || true)"

# Issue #44's second Done-when case, gate side: a pull request whose only
# protected touch is a changed test file is classified as clearable, so a
# green inspector-tests status lets it through without the Client's override.
assert_eq "a pull request adding only a test file yields one clearable match" \
  "$(printf 'test\tpkg/rounding_test.go')" \
  "$(classify_protected_matches "$(printf 'pkg/rounding_test.go\t\npkg/rounding.go\t')" "$ALWAYS_PROTECTED" "$project_paths_fixture" "$test_patterns_fixture")"

# Issue #44's first Done-when case, gate side: the deleted test is still a
# touch, so it is still a match - it just becomes the one the inspector-tests
# status decides, and that status is a failure when the exam was weakened.
assert_eq "a deleted test file is a clearable match, decided by the test status" \
  "$(printf 'test\tfeature_test.go')" \
  "$(classify_protected_matches "$(printf 'feature_test.go\t')" "$ALWAYS_PROTECTED" "$project_paths_fixture" "$test_patterns_fixture")"

assert_eq "every protected path is reported, not just the first" \
  "$(printf 'blocked\tscript/check\ntest\tpkg/a_test.go')" \
  "$(classify_protected_matches "$(printf 'script/check\t\npkg/a_test.go\t\nREADME.md\t')" "$ALWAYS_PROTECTED" "$project_paths_fixture" "$test_patterns_fixture")"

assert_eq "a clean pull request yields no matches at all" \
  "" \
  "$(classify_protected_matches "$touched_clean" "$ALWAYS_PROTECTED" "$project_paths_fixture" "$test_patterns_fixture")"

# Renaming a protected test away is a touch of the old path, so it is
# classified from previous_filename rather than vanishing.
assert_eq "renaming a protected test away is caught via previous_filename" \
  "$(printf 'test\tfeature_test.go')" \
  "$(classify_protected_matches "$(printf 'renamed.go\tfeature_test.go')" "$ALWAYS_PROTECTED" "$project_paths_fixture" "$test_patterns_fixture")"

# --- listing_count_check ---

assert_eq "matching counts are ok" "ok" "$(listing_count_check 3 3)"
assert_eq "3000 listed against a larger declared count is the API's hard cap" \
  "cap" "$(listing_count_check 3000 3001)"
assert_eq "any other mismatch is a mid-run push race" \
  "race" "$(listing_count_check 5 6)"
assert_eq "a mismatch below 3000 is a race, not a cap, even at the boundary" \
  "race" "$(listing_count_check 2999 3000)"

# --- gh_api_failure_reason ---
#
# An ::error:: annotation is one line, so a multi-line gh error has to be
# joined rather than truncated at the first newline, and an empty capture
# still has to say something.

assert_eq "a single-line gh error comes through as-is" \
  "gh: HTTP 403: API rate limit exceeded" \
  "$(gh_api_failure_reason "gh: HTTP 403: API rate limit exceeded" 1)"

assert_eq "a multi-line gh error is joined onto one line" \
  "gh: HTTP 403; API rate limit exceeded; try again later" \
  "$(gh_api_failure_reason "$(printf 'gh: HTTP 403\nAPI rate limit exceeded\ntry again later')" 1)"

assert_eq "blank lines and surrounding whitespace are dropped from the join" \
  "gh: HTTP 502; Bad Gateway" \
  "$(gh_api_failure_reason "$(printf '  gh: HTTP 502  \n\n\tBad Gateway\n\n')" 1)"

assert_eq "a silent failure still names the exit status instead of trailing off" \
  "gh exited 7 without printing a reason" \
  "$(gh_api_failure_reason "" 7)"

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
  local desc="$1" raw="$2" expected_substring="$3" gh_error="${4:-}"
  local out rc=0
  out=$(protected_paths_from_response "$raw" "$gh_error") || rc=$?
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

# When the caller has gh's own stderr, the reason is gh's words rather
# than a guess between the several ways "no response" can happen.
assert_fails_closed "no response at all names gh's own reason when the caller has it" \
  "" \
  "no HTTP response at all: dial tcp: lookup api.github.com: no such host" \
  "dial tcp: lookup api.github.com: no such host"

assert_fails_closed "an empty gh reason falls back to naming the possibilities" \
  "" \
  "a network failure, or gh could not reach GitHub" \
  ""

assert_fails_closed "a reply that is not an HTTP response fails closed" \
  "gh: could not resolve host" \
  "did not come back as an HTTP response"

assert_fails_closed "empty content (the API's 1MB inline limit) fails closed" \
  "$(contents_response "200 OK" '{"content":"","encoding":"none"}')" \
  "empty content"

# The three ways a 200's content can be unreadable are distinct reasons,
# not all "empty content" blamed on the 1MB inline limit.

assert_fails_closed "a 200 whose body is not JSON says so, rather than blaming the 1MB limit" \
  "$(contents_response "200 OK" '{"content": truncated mid-resp')" \
  "not readable JSON"

assert_fails_closed "content that is not valid base64 says so, rather than blaming the 1MB limit" \
  "$(contents_response "200 OK" '{"content":"!!! not base64 !!!","encoding":"base64"}')" \
  "not valid base64"

# "Cg==" is a single newline: real base64, decodes fine, but there is no
# file there to read paths out of.
assert_fails_closed "content that decodes to an empty file says so, rather than blaming the 1MB limit" \
  "$(contents_response "200 OK" '{"content":"Cg==","encoding":"base64"}')" \
  "decoded to an empty file"

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

# --- test_paths_from_response ---
#
# The same reader, one field over (issue #44). testPaths is what decides
# which protected path a green inspector-tests status may clear, so it fails
# closed on exactly the same things protectedPaths does - a testPaths read
# that quietly came back empty would leave a changed test blocked forever,
# and one that quietly came back wrong would clear the wrong file.

assert_test_patterns() {
  local desc="$1" raw="$2" expected="$3"
  local out rc=0
  out=$(test_paths_from_response "$raw") || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "FAIL: $desc - failed closed instead of succeeding: $out"
    failures=$((failures + 1))
    return
  fi
  assert_eq "$desc" "$expected" "$out"
}

assert_test_fails_closed() {
  local desc="$1" raw="$2" expected_substring="$3"
  local out rc=0
  out=$(test_paths_from_response "$raw") || rc=$?
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

assert_test_patterns "testPaths entries come back one per line" \
  "$(inspector_json_response '{"protectedPaths": ["*_test.go"], "testPaths": ["*_test.go", "spec/**"]}')" \
  "$(printf '*_test.go\nspec/**')"

assert_test_patterns "a config with no testPaths declares no test paths" \
  "$(inspector_json_response '{"protectedPaths": ["*_test.go"]}')" \
  ""

assert_test_patterns "a 404 declares no test paths, the same safe absence" \
  "$(contents_response "404 Not Found" '{"message":"Not Found"}')" \
  ""

assert_test_fails_closed "testPaths present but not an array fails closed, naming that field" \
  "$(inspector_json_response '{"testPaths": "*_test.go"}')" \
  "its \"testPaths\" is a JSON string, not an array"

assert_test_fails_closed "a testPaths entry that is not a string fails closed, naming that field" \
  "$(inspector_json_response '{"testPaths": ["ok_test.go", 7]}')" \
  "one of its \"testPaths\" entries is not a string"

assert_test_fails_closed "an unreadable response fails closed for testPaths too" \
  "$(contents_response "403 Forbidden" '{"message":"API rate limit exceeded"}')" \
  "HTTP 403"

# The two fields are read independently: a broken protectedPaths must not
# be silently tolerated because testPaths happened to be fine, nor the
# reverse. Each read names its own field.
assert_fails_closed "a broken protectedPaths still fails closed when testPaths is valid" \
  "$(inspector_json_response '{"protectedPaths": "script/check", "testPaths": ["*_test.go"]}')" \
  "its \"protectedPaths\" is a JSON string, not an array"

# --- main(), against a stubbed gh (issue #21) ---
#
# Everything above this line takes plain strings in and prints plain strings
# out. main() is the part that sequences the three GitHub API reads and
# decides what to do with each answer, and nothing in the repository used to
# exercise it: PR #19's validation built a driver that replayed scenarios
# against a fixture-serving `gh` stub, off-network, it passed, and it was
# never committed - so the orchestration was proven once, in a pipeline run's
# scratch space. This is that driver, committed, and shaped to this file
# rather than built as a separate harness, so the code under test is the
# inspector-gate.sh sourced at the top of this file and nothing else.
#
# `gh` is stubbed as a shell FUNCTION, not as an executable on PATH.
# gh_api_call invokes `gh api ...` as a command, so a function by that name
# is what it finds, and no PATH is rearranged - which matters, because the
# drift check below re-runs this whole file with PATH cut down to exactly
# REQUIRED_TOOLS, and a stub script would need its own interpreter on that
# PATH just to be found. Either shape has to emulate gh's --jq and
# --paginate itself (the stub prints the TSV main() would have been handed),
# so the function costs nothing in fidelity.
#
# What this proves that the pure functions cannot: which endpoints are read
# and in what order, that a read failure stops the run instead of falling
# through to a verdict, that both commit statuses come out of ONE statuses
# call and both config fields out of ONE contents call, that the base ref
# goes in as a parameter rather than pasted into a query string, that the
# project's own declared paths actually reach the decision, and that
# question 2 still runs and still reports when question 1 is already red.
#
# Every fixture here is shaped like the real response: the statuses list
# newest-first and not deduped by context, a rename carrying only
# previous_filename for its old path, the Contents API's base64 body - and
# gh's own non-zero exit on a 404, which is why main() may never decide
# anything from that call's exit status.

GH_CALL_LOG=""
GH_CALL_SEQ=""

# gh stands in for the real command for every call main() makes, and records
# each one twice: the full argument list, for asserting what was asked, and a
# short label, for asserting the order. Dispatch is on the request path, so a
# call main() grows later is named as unexpected rather than quietly answered
# with some other call's fixture.
gh() {
  local label rc=0 body="" stderr_text=""
  printf '%s\n' "$*" >>"$GH_CALL_LOG"
  case "$*" in
  *"/statuses/"*)
    label=statuses
    body="$STUB_STATUSES"
    rc="$STUB_STATUSES_RC"
    stderr_text="$STUB_STATUSES_ERR"
    ;;
  *"/pulls/"*"/files"*)
    label=files
    body="$STUB_FILES"
    rc="$STUB_FILES_RC"
    stderr_text="$STUB_FILES_ERR"
    ;;
  *"/contents/.inspector.json"*)
    label=config
    body="$STUB_CONFIG"
    rc="$STUB_CONFIG_RC"
    stderr_text="$STUB_CONFIG_ERR"
    ;;
  *)
    label=unexpected
    stderr_text="inspector-gate_test: no fixture for this call"
    rc=64
    ;;
  esac
  printf '%s\n' "$label" >>"$GH_CALL_SEQ"
  if [ -n "$body" ]; then
    printf '%s\n' "$body"
  fi
  if [ -n "$stderr_text" ]; then
    printf '%s\n' "$stderr_text" >&2
  fi
  return "$rc"
}

# scenario resets every input to one clean-and-green pull request, so each
# case below changes only the one thing it is about. The project config it
# serves is this repository's own shape: a protected check command and
# protected tests, with the tests also declared as testPaths.
scenario() {
  GH_TOKEN="stub-token"
  REPO="inactdev/widget"
  PR_NUMBER="7"
  HEAD_SHA="c0ffee11c0ffee22c0ffee33c0ffee44c0ffee55"
  BASE_REF="main"
  STATUS_CONTEXT="inspector"
  TESTS_STATUS_CONTEXT="inspector-tests"
  CHANGED_FILES=2

  STUB_STATUSES=$'inspector\tsuccess'
  STUB_STATUSES_RC=0
  STUB_STATUSES_ERR=""
  STUB_FILES=$'src/main.go\t\nREADME.md\t'
  STUB_FILES_RC=0
  STUB_FILES_ERR=""
  STUB_CONFIG=$(inspector_json_response '{"check": "script/check", "protectedPaths": ["script/check", "*_test.go"], "testPaths": ["*_test.go"]}')
  STUB_CONFIG_RC=0
  STUB_CONFIG_ERR=""
}

# run_main runs one scenario and leaves its results in MAIN_RC (main() exits
# 1 on red, so it is run in a command substitution), MAIN_OUT (stdout and
# stderr together), MAIN_CALLS and MAIN_SEQ. The two logs are files because
# the stub runs inside that command substitution's subshell, where a variable
# it set would not survive.
run_main() {
  GH_CALL_LOG=$(mktemp)
  GH_CALL_SEQ=$(mktemp)
  MAIN_RC=0
  MAIN_OUT=$(main 2>&1) || MAIN_RC=$?
  MAIN_CALLS=$(cat "$GH_CALL_LOG")
  MAIN_SEQ=$(cat "$GH_CALL_SEQ")
  rm -f "$GH_CALL_LOG" "$GH_CALL_SEQ"
}

# Captured gate output is only ever shown with its "::" chewed up. These runs
# print real ::error:: lines, and this file's own output is read by GitHub
# Actions whenever script/check runs there - a fixture's error must not arrive
# as a genuine annotation on inspector's own CI run.
show_main_out() {
  printf '  the gate said:\n%s\n' "${MAIN_OUT//::/--}"
}

assert_rc() {
  local desc="$1" want="$2"
  if [ "$MAIN_RC" = "$want" ]; then
    echo "ok: $desc"
  else
    echo "FAIL: $desc - the gate exited $MAIN_RC, expected $want"
    show_main_out
    failures=$((failures + 1))
  fi
}

assert_said() {
  local desc="$1" needle="$2"
  case "$MAIN_OUT" in
  *"$needle"*) echo "ok: $desc" ;;
  *)
    echo "FAIL: $desc - the gate never said \"${needle//::/--}\""
    show_main_out
    failures=$((failures + 1))
    ;;
  esac
}

assert_asked() {
  local desc="$1" needle="$2"
  case "$MAIN_CALLS" in
  *"$needle"*) echo "ok: $desc" ;;
  *)
    echo "FAIL: $desc - no call contained \"$needle\""
    echo "  the calls made were:"
    printf '%s\n' "$MAIN_CALLS"
    failures=$((failures + 1))
    ;;
  esac
}

assert_never_asked() {
  local desc="$1" needle="$2"
  case "$MAIN_CALLS" in
  *"$needle"*)
    echo "FAIL: $desc - a call contained \"$needle\" and must not have"
    echo "  the calls made were:"
    printf '%s\n' "$MAIN_CALLS"
    failures=$((failures + 1))
    ;;
  *) echo "ok: $desc" ;;
  esac
}

# Clean and green: the whole sequence runs, in order, and the run is green.

scenario
run_main
assert_rc "clean and green: the gate exits 0" 0
assert_said "clean and green: question 1 reports green" "inspector status: green."
assert_said "clean and green: question 2 reports nothing touched" "protected files: none touched."
assert_said "clean and green: the run states its own verdict" "inspector-gate: green."
assert_eq "clean and green: statuses, then the changed files, then the config - three reads, in that order" \
  "$(printf 'statuses\nfiles\nconfig')" "$MAIN_SEQ"

# The commit's statuses come from the paginated LIST endpoint. Never the
# combined commits/{sha}/status one, whose statuses array stops at 30 with no
# pagination of its own, so a green "inspector" can silently fall off the page
# and read as "no status posted at all".
assert_asked "the statuses list endpoint is read, paginated" \
  "repos/$REPO/statuses/$HEAD_SHA?per_page=100 --paginate"
assert_never_asked "the combined commit-status endpoint is never used" \
  "commits/$HEAD_SHA/status"
assert_asked "the changed-file list is read from the pull request, paginated" \
  "repos/$REPO/pulls/$PR_NUMBER/files?per_page=100 --paginate"
assert_asked "the config is read from the base branch tip, with the HTTP status line included" \
  "--include repos/$REPO/contents/.inspector.json -f ref=main"
assert_never_asked "the base ref is never pasted into the query string" \
  "contents/.inspector.json?ref"
assert_said "the run logs the project's own protected entries, not just the floor" \
  "$(printf '  script/check')"
assert_said "the run logs which patterns a test status may clear" \
  "test patterns in effect (clearable by a green \"inspector-tests\" status):"

# A branch name git allows but a URL does not. Interpolated into the query
# string, "release&hotfix" would send ref=release and read a DIFFERENT
# branch's protected-path list - a quiet fail-open in the one read this gate
# must get right. It goes in as a -f parameter, which gh percent-encodes.
scenario
BASE_REF='release&hotfix'
run_main
assert_rc "a base branch whose name contains & is still judged" 0
assert_asked "...and its name goes in as a parameter, whole" "-f ref=release&hotfix"
assert_never_asked "...never as part of the request path" "contents/.inspector.json?"

# An empty pull request: nothing listed, nothing declared changed.
scenario
STUB_FILES=""
CHANGED_FILES=0
run_main
assert_rc "a pull request that changes no files at all is green" 0
assert_said "...and touches nothing protected" "protected files: none touched."

# Question 1: absent, failing, and stale statuses.

scenario
STUB_STATUSES=""
run_main
assert_rc "no status posted to the head commit: red" 1
assert_said "...naming the context nobody posted" \
  "no \"inspector\" status has been posted to this commit at all"
assert_said "...and question 2 still ran and still reported" "protected files: none touched."
assert_eq "...all three reads still happen - one red question does not end the run" \
  "$(printf 'statuses\nfiles\nconfig')" "$MAIN_SEQ"

scenario
STUB_STATUSES=$'inspector\tfailure'
run_main
assert_rc "a failing inspector status: red" 1
assert_said "...not conflated with an absent one" \
  "the \"inspector\" status on this commit is \"failure\", not \"success\""

scenario
STUB_STATUSES=$'inspector\tsuccess\nexaminer\tfailure\ninspector\tfailure'
run_main
assert_rc "an older failing status does not shadow the newest success" 0

# The gap the removed `on: status` trigger was built for: a commit status can
# only be posted to a commit that is already on the remote, so the push's own
# run can honestly see none. There is no status-event path left to exercise -
# that trigger associated its run with the default branch's tip rather than
# the pull request's head, and was removed (the workflow facts below pin its
# absence); inspector#18 closed the gap by having inspector stage and stamp a
# commit before it updates the pull request branch. What is still worth
# proving is the only thing a re-run depends on: the verdict follows the
# status that is on the head commit NOW, not the one the first run saw.
scenario
STUB_STATUSES=""
run_main
status_arrived_late_first_rc="$MAIN_RC"
scenario # the same pull request, the same head commit, the status has landed
run_main
assert_eq "the same head commit is red while its status has not landed yet" \
  "1" "$status_arrived_late_first_rc"
assert_rc "...and green on a re-run once it has, with nothing else changed" 0

# Question 2: protected paths, from the floor and from the project's own list.

scenario
STUB_FILES=$'.github/workflows/inspector-gate.yml\t\nsrc/main.go\t'
run_main
assert_rc "touching a workflow file is red even on a green status" 1
assert_said "...the green status is still reported, so the log says which question failed" \
  "inspector status: green."
assert_said "...and the touched file is named" \
  "touches \".github/workflows/inspector-gate.yml\", protected file(s)"

scenario
STUB_FILES=$'script/check\t\nsrc/main.go\t'
run_main
assert_rc "touching a path the project's own .inspector.json protects is red" 1
assert_said "...so the fetched config's entries really do reach the decision" \
  "touches \"script/check\", protected file(s)"

scenario
STUB_FILES=$'renamed.go\tscript/check'
CHANGED_FILES=1
run_main
assert_rc "renaming a protected file away is red" 1
assert_said "...caught by its previous_filename, which is the only place the old path appears" \
  "touches \"script/check\", protected file(s)"

# Both questions red in the same run. main() accumulates rather than exiting
# at the first failure, so one run tells a worker everything it has to fix.
scenario
STUB_STATUSES=""
STUB_FILES=$'.inspector.json\t'
CHANGED_FILES=1
run_main
assert_rc "both questions red: red" 1
assert_said "both questions red: question 1's reason is reported" \
  "no \"inspector\" status has been posted"
assert_said "both questions red: question 2's reason is reported too, in the same run" \
  "touches \".inspector.json\", protected file(s)"

# Every read fails closed, and says which read failed rather than letting a
# rate limit read exactly like a real red.

scenario
STUB_STATUSES=""
STUB_STATUSES_RC=1
STUB_STATUSES_ERR="gh: HTTP 403: API rate limit exceeded"
run_main
assert_rc "a failed statuses read fails closed" 1
assert_said "...naming which read failed" "Could not read this commit's commit statuses"
assert_said "...and quoting gh's own reason instead of guessing" "API rate limit exceeded"
assert_eq "...before anything else is asked" "statuses" "$MAIN_SEQ"

scenario
STUB_FILES=""
STUB_FILES_RC=1
STUB_FILES_ERR="gh: HTTP 502: Bad Gateway"
run_main
assert_rc "a failed changed-file read fails closed rather than passing unchecked" 1
assert_said "...naming which read failed" "Could not read this pull request's changed-file list"
assert_eq "...and the config is never read" "$(printf 'statuses\nfiles')" "$MAIN_SEQ"

scenario
STUB_CONFIG=$(contents_response "403 Forbidden" '{"message":"API rate limit exceeded"}')
STUB_CONFIG_RC=1
run_main
assert_rc "an unreadable .inspector.json fails closed, never falling back to the floor" 1
assert_said "...naming what could not be checked" \
  "cannot be checked against the protected paths this project actually declares"
assert_said "...and why" "HTTP 403"

# No response at all - DNS, an unreachable GitHub, a gh crash - is the one
# case where gh's own stderr is the only thing that can say which failure it
# was, so main() hands it to the reader rather than letting all three look
# identical from an empty string.
scenario
STUB_CONFIG=""
STUB_CONFIG_RC=1
STUB_CONFIG_ERR="dial tcp: lookup api.github.com: no such host"
run_main
assert_rc "a config read that produced no response at all fails closed" 1
assert_said "...and names gh's own reason rather than guessing between them" \
  "no HTTP response at all: dial tcp: lookup api.github.com: no such host"

# The two config fields are read independently off the same response, so a
# readable protectedPaths does not excuse an unreadable testPaths.
scenario
STUB_CONFIG=$(inspector_json_response '{"protectedPaths": ["script/check"], "testPaths": "*_test.go"}')
run_main
assert_rc "a readable protectedPaths with a broken testPaths still fails closed" 1
assert_said "...naming the test question rather than the protected-path one" \
  "which of this pull request's protected files are this project's own tests is unknown"

# A 404 is the one safe absence: a project that has never adopted
# .inspector.json gets the ALWAYS_PROTECTED floor and no exception. gh exits
# non-zero on it, which is exactly why main() decides on the body.
scenario
STUB_CONFIG=$(contents_response "404 Not Found" '{"message":"Not Found"}')
STUB_CONFIG_RC=1
run_main
assert_rc "a project with no .inspector.json at all is judged on the floor alone" 0
assert_said "...and is told no protected path can be cleared by a test status" \
  "none declared, so no protected path can be cleared"

scenario
STUB_CONFIG=$(contents_response "404 Not Found" '{"message":"Not Found"}')
STUB_CONFIG_RC=1
STUB_FILES=$'Dockerfile.inspector\t'
CHANGED_FILES=1
run_main
assert_rc "...while the floor still blocks Dockerfile.inspector with no config to read" 1
assert_said "...naming it" "touches \"Dockerfile.inspector\", protected file(s)"

# An incomplete changed-file listing is never judged, whichever way it is
# incomplete.

scenario
CHANGED_FILES=3
run_main
assert_rc "a changed-file count that moved mid-run fails closed" 1
assert_said "...as a mid-run push, not a listing problem" \
  "changed while this check was running"
assert_eq "...before the config is read" "$(printf 'statuses\nfiles')" "$MAIN_SEQ"

scenario
STUB_FILES=$(awk 'BEGIN { for (i = 1; i <= 3000; i++) printf "f%d.txt\t\n", i }')
CHANGED_FILES=3001
run_main
assert_rc "the API's silent 3000-file listing cap fails closed" 1
assert_said "...naming the cap rather than blaming a race" \
  "The API caps that listing at 3000 files"

# Issue #44's stopgap, end to end: a protected path the project itself
# declares a test is cleared by a green inspector-tests status, and by
# nothing else. Both statuses come out of the ONE statuses read, so the two
# questions can never be answered from two different moments.

scenario
STUB_FILES=$'pkg/rounding_test.go\t\npkg/rounding.go\t'
STUB_STATUSES=$'inspector-tests\tsuccess\ninspector\tsuccess'
run_main
assert_rc "a changed protected test file passes on a green inspector-tests status" 0
assert_said "...and the file it cleared is named" \
  "cleared by a green \"inspector-tests\" status: \"pkg/rounding_test.go\""
assert_eq "...both statuses read from one call, both config fields from one call" \
  "$(printf 'statuses\nfiles\nconfig')" "$MAIN_SEQ"

scenario
STUB_FILES=$'pkg/rounding_test.go\t\npkg/rounding.go\t'
run_main
assert_rc "the same change is red when no inspector-tests status was posted" 1
assert_said "...naming that context, not the other one" \
  "no \"inspector-tests\" status has been posted to this commit at all"
assert_said "...and the test file waiting on it" \
  "changes \"pkg/rounding_test.go\", protected test file(s)"

scenario
STUB_FILES=$'pkg/rounding_test.go\t\npkg/rounding.go\t'
STUB_STATUSES=$'inspector\tsuccess\ninspector-tests\tfailure'
run_main
assert_rc "a failing inspector-tests status does not clear a protected test" 1
assert_said "...naming its state" \
  "the \"inspector-tests\" status on this commit is \"failure\", not \"success\""

scenario
STUB_FILES=$'pkg/rounding_test.go\t\nscript/check\t'
STUB_STATUSES=$'inspector\tsuccess\ninspector-tests\tsuccess'
run_main
assert_rc "a blocked path alongside a clearable test is still red" 1
assert_said "...the blocked one is reported" "touches \"script/check\", protected file(s)"
assert_said "...and the cleared one is reported separately" \
  "cleared by a green \"inspector-tests\" status: \"pkg/rounding_test.go\""

# The floor outranks a project's own testPaths, through main() and not only
# in protected_match_kind: otherwise a project could declare ".github/**" a
# test path and hand the gate's own definition to a model's judgment.
scenario
STUB_CONFIG=$(inspector_json_response '{"protectedPaths": [".github/**"], "testPaths": [".github/**"]}')
STUB_FILES=$'.github/scripts/inspector-gate.sh\t'
CHANGED_FILES=1
STUB_STATUSES=$'inspector\tsuccess\ninspector-tests\tsuccess'
run_main
assert_rc "a project cannot declare the gate's own files tests and clear them" 1
assert_said "...they stay blocked outright" \
  "touches \".github/scripts/inspector-gate.sh\", protected file(s)"

# A missing environment variable is refused on main()'s first line, before
# any credential is spent on a read that could not be judged anyway.
scenario
unset BASE_REF
run_main
assert_rc "a missing environment variable refuses the run" 1
assert_eq "...having asked GitHub nothing at all" "" "$MAIN_SEQ"

# --- the workflow's own facts (issue #21) ---
#
# main() is only ever reached through .github/workflows/inspector-gate.yml,
# and part of what the orchestration depends on lives there rather than in
# the script: which events wake it, which it must never be woken by, and
# every environment variable it requires. No scenario above can see any of
# that - a workflow that stopped setting TESTS_STATUS_CONTEXT would abort
# main() on its first line and still pass every one of them. So it is read as
# text, the same way gate-sync_test.sh reads its own workflow.

WORKFLOW="../workflows/inspector-gate.yml"
workflow_text=$(cat "$WORKFLOW")

assert_contains() {
  local desc="$1" haystack="$2" needle="$3"
  case "$haystack" in
  *"$needle"*) echo "ok: $desc" ;;
  *)
    echo "FAIL: $desc - \"$needle\" is not in $WORKFLOW"
    failures=$((failures + 1))
    ;;
  esac
}

assert_not_contains() {
  local desc="$1" haystack="$2" needle="$3"
  case "$haystack" in
  *"$needle"*)
    echo "FAIL: $desc - \"$needle\" is in $WORKFLOW and must not be"
    failures=$((failures + 1))
    ;;
  *) echo "ok: $desc" ;;
  esac
}

assert_contains "the gate runs on pull_request_target, so its definition comes from the base branch" \
  "$workflow_text" "pull_request_target:"

# `edited` is in the list because retargeting a pull request to another base
# branch is an edited event, and the base is what selects which
# .inspector.json the config read above is answered from.
assert_contains "all four pull request event types are listed explicitly" \
  "$workflow_text" "types: [opened, synchronize, reopened, edited]"

# The status-event path, which is what is left of it. An `on: status` trigger
# was built and removed: such a run's check attaches to the default branch's
# tip rather than the pull request's head, and a status event cannot be
# filtered by context, so every third party's status re-ran the whole gate
# and landed its failures on the default branch. Its absence is a decision,
# so it is pinned like one.
status_trigger=$(awk '$1 == "status:" { print "a status trigger is back" }' "$WORKFLOW")
assert_eq "no status trigger: such a run judges the default branch's tip, never the pull request's head" \
  "" "$status_trigger"

# No step may check the pull request out - that is the vulnerability class
# pull_request_target is known for, and SPEC.md section 8 rules it out. Read
# as "this workflow uses no action at all", which is both the actual state
# and the only form of this claim that cannot be satisfied by a comment: the
# file's own header says "never add `actions/checkout`" in prose, so a
# substring search for that name finds it and proves nothing.
action_step=$(awk '$1 == "uses:" || ($1 == "-" && $2 == "uses:") { print "an action is used" }' "$WORKFLOW")
assert_eq "the workflow runs no action at all, so nothing in it can check the pull request out" \
  "" "$action_step"

assert_contains "it loads its own definition from the base branch, as an encoded parameter" \
  "$workflow_text" 'contents/.github/scripts/inspector-gate.sh" -f ref="$BASE_REF"'

assert_contains "it only reads statuses" "$workflow_text" "statuses: read"
assert_not_contains "it is never allowed to write one" "$workflow_text" "statuses: write"

# Every name main() requires, taken from main()'s own require line rather
# than copied, then checked against the workflow. The list is also pinned, so
# adding a required variable fails here - which is the reminder to supply it
# from the workflow and to set it in scenario() above.
required_env=$(awk -F'[{}]' '/^  : "/ {
  for (i = 2; i <= NF; i += 2) {
    name = $i
    sub(/:\?$/, "", name)
    print name
  }
}' ./inspector-gate.sh)

assert_eq "main() requires exactly the environment the workflow was written against" \
  "$(printf 'GH_TOKEN\nREPO\nPR_NUMBER\nHEAD_SHA\nBASE_REF\nCHANGED_FILES\nSTATUS_CONTEXT\nTESTS_STATUS_CONTEXT')" \
  "$required_env"

for env_name in $required_env; do
  if awk -v want="$env_name:" '$1 == want { found = 1 } END { exit found ? 0 : 1 }' "$WORKFLOW"; then
    echo "ok: the workflow supplies $env_name to main()"
  else
    echo "FAIL: main() requires $env_name and $WORKFLOW never sets it - every run would abort on main()'s first line"
    failures=$((failures + 1))
  fi
done

# The two context strings are the contract with the local half: inspector
# posts "inspector" (SPEC.md section 7) and "inspector-tests"
# (inspector.TestsStatusContext). A name that does not match reads as "no
# status posted", which keeps every pull request red. Compared as whole
# fields, because "STATUS_CONTEXT: inspector" is a substring of the
# TESTS_STATUS_CONTEXT line and a substring test would pass with the first
# one deleted.
assert_workflow_env_value() {
  local desc="$1" name="$2" want="$3"
  local got
  got=$(awk -v n="$name:" '$1 == n { print $2 }' "$WORKFLOW")
  assert_eq "$desc" "$want" "$got"
}

assert_workflow_env_value "the status context is the stable string inspector-gate keys on" \
  "STATUS_CONTEXT" "inspector"
assert_workflow_env_value "the test status context matches inspector.TestsStatusContext" \
  "TESTS_STATUS_CONTEXT" "inspector-tests"

# --- required-tools drift ---
#
# script/check's preflight (.github/scripts/required-tools.sh) is a
# hand-maintained list, and a hand-maintained list drifts from what the
# code it guards actually calls - that already happened once ("tr" was a
# real dependency this file and inspector-gate.sh both use, and it wasn't
# on the list). Rather than re-parse this file's bash for external
# command names - a naive scanner also matches function calls, keywords,
# and variable expansions, which makes "simple and reliable" the wrong
# combination to promise - this proves the claim mechanically: it
# re-executes this whole test file, unmodified, with PATH restricted to
# EXACTLY REQUIRED_TOOLS's tools, and fails loudly, naming what broke, if
# any assertion above needs something that isn't on the list. A recursion
# guard (INSPECTOR_GATE_DRIFT_CHECK) stops the re-executed copy from
# spawning a third layer.
#
# Since issue #21 this reaches main() too, because the scenarios above do:
# the restricted run exercises the gate's whole decision path with a stubbed
# gh, so main()'s own external tools are now covered by the same mechanism.
# That is how "cat" and "wc" were found missing from the list - gh_api_call
# reads its captured stderr with one and main() counts the changed-file
# listing with the other, and neither was named for as long as nothing ran
# main(). What it still cannot reach is the real `gh` binary, which the
# scenarios replace with a function and which no check on this machine runs.
#
# It also cannot cover its OWN scaffolding: ln and sed below run in the
# outer, unrestricted-PATH copy of this file (and sed only when there is a
# failure to print), and only the inner re-execution has PATH restricted, so
# no failure of theirs can ever reach the restricted run. They are named in
# REQUIRED_TOOLS directly instead. Proving them would need a second,
# different mechanism checking the checker, which is not worth building for
# two coreutils that are safe to just require outright - script/check cannot
# finish without them either way, and the point of the preflight is that it
# says so by name. mktemp and rm used to be in that company and no longer
# are: the main() harness and gh_api_call both use them inside the
# restricted run, which exercises them for real.
if [ -z "${INSPECTOR_GATE_DRIFT_CHECK:-}" ]; then
  # shellcheck source=required-tools.sh
  source ./required-tools.sh

  drift_bin=$(mktemp -d)
  trap 'rm -rf "$drift_bin"' EXIT
  drift_missing=""
  for tool in $REQUIRED_TOOLS; do
    tool_path=$(command -v "$tool") || { drift_missing="$drift_missing $tool"; continue; }
    ln -s "$tool_path" "$drift_bin/$tool"
  done

  if [ -n "$drift_missing" ]; then
    echo "FAIL: required-tools drift check could not even locate:$drift_missing (present in REQUIRED_TOOLS but not on this machine's own PATH)"
    failures=$((failures + 1))
  else
    real_bash=$(command -v bash)
    drift_log=$(mktemp)
    # Not "$0": this script already cd'd to its own directory at the top,
    # so $0 (whatever relative or absolute form it was invoked with) may
    # no longer resolve from here. It always IS this file, right here.
    if PATH="$drift_bin" INSPECTOR_GATE_DRIFT_CHECK=1 "$real_bash" ./inspector-gate_test.sh > "$drift_log" 2>&1; then
      echo "ok: every tool this file's own tests actually use is covered by REQUIRED_TOOLS"
    else
      echo "FAIL: this test file fails when PATH is restricted to exactly REQUIRED_TOOLS - something it (or inspector-gate.sh) calls is missing from .github/scripts/required-tools.sh:"
      sed 's/^/  /' "$drift_log"
      failures=$((failures + 1))
    fi
    rm -f "$drift_log"
  fi
fi

echo
if [ "$failures" -ne 0 ]; then
  echo "$failures assertion(s) failed."
  exit 1
fi
echo "all assertions passed."
