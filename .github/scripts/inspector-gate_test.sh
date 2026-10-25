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
# This cannot, and does not try to, cover main()'s live GitHub API
# orchestration - consistent with this file's header, that code path is
# only exercised inside Actions, never by anything script/check runs.
#
# It also cannot cover its OWN scaffolding: mktemp, ln, sed and rm below
# run in the outer, unrestricted-PATH copy of this file, and only the
# inner re-execution has PATH restricted, so no failure of theirs can ever
# reach the restricted run. They are named in REQUIRED_TOOLS directly
# instead. Proving them would need a second, different mechanism checking
# the checker, which is not worth building for four coreutils that are
# safe to just require outright - script/check cannot finish without them
# either way, and the point of the preflight is that it says so by name.
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
