#!/usr/bin/env bash
# Tests for gate-sync.sh, in two halves.
#
# The first half unit tests its pure functions (parse_consumers,
# contents_base64_from_response, missing_env, api_failure_reason, file_mode,
# and the three that build the commit and pull request text). These take
# plain strings in and print plain strings out - no network, no GitHub API -
# so they can run anywhere, including here.
#
# The second half proves the FACTS OF THE WORKFLOW by reading
# .github/workflows/gate-sync.yml as text: what wakes it up, what it refuses
# to be woken by, which secret it runs with, what it is allowed to write, and
# that every environment variable the script requires is actually supplied by
# it. None of that is reachable from a unit test of the script alone - a
# workflow whose `paths:` filter no longer names a synced file is a sync that
# simply never runs, and passes every other test in this file.
#
# What this deliberately does NOT cover: the GitHub API orchestration in
# main() - reading each consumer's current copy, writing the blob, tree,
# commit and ref, opening or updating the pull request. That is a gap, not a
# proof of anything; see gate-sync.sh's header. inspector-gate_test.sh used
# to record the same limitation and no longer does - issue #21 closed it
# there by stubbing `gh` as a shell function and replaying scenarios through
# that gate's main(). The same shape would work here, on a bigger surface:
# this script's main() writes rather than reads, so its fixtures have to
# cover a sequence of mutations and their idempotence, which is why it has
# not simply been copied over.
#
# Run directly: .github/scripts/gate-sync_test.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

# shellcheck source=.github/scripts/gate-sync.sh
source .github/scripts/gate-sync.sh

WORKFLOW=".github/workflows/gate-sync.yml"

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

assert_contains() {
  local desc="$1" haystack="$2" needle="$3"
  case "$haystack" in
  *"$needle"*) echo "ok: $desc" ;;
  *)
    echo "FAIL: $desc - \"$needle\" is not there"
    failures=$((failures + 1))
    ;;
  esac
}

assert_not_contains() {
  local desc="$1" haystack="$2" needle="$3"
  case "$haystack" in
  *"$needle"*)
    echo "FAIL: $desc - \"$needle\" is there and should not be"
    failures=$((failures + 1))
    ;;
  *) echo "ok: $desc" ;;
  esac
}

# --- api_failure_reason ---
#
# A copy of inspector-gate.sh's helper rather than a shared one (see
# gate-sync.sh's own note on why), so it is tested here too rather than
# assumed to still behave like the original.

assert_eq "a single-line gh error comes through as-is" \
  "gh: HTTP 403: API rate limit exceeded" \
  "$(api_failure_reason "gh: HTTP 403: API rate limit exceeded" 1)"

assert_eq "a multi-line gh error is joined onto one line" \
  "gh: HTTP 422; Reference update failed" \
  "$(api_failure_reason "$(printf 'gh: HTTP 422\nReference update failed')" 1)"

assert_eq "a silent failure still names the exit status instead of trailing off" \
  "gh exited 7 without printing a reason" \
  "$(api_failure_reason "" 7)"

# --- missing_env ---

export GATE_SYNC_TEST_SET="a value"
export GATE_SYNC_TEST_EMPTY=""
unset GATE_SYNC_TEST_UNSET 2>/dev/null || true

assert_eq "nothing is missing when every name is set" \
  "" \
  "$(missing_env "GATE_SYNC_TEST_SET")"

assert_eq "an unset variable is named" \
  "GATE_SYNC_TEST_UNSET" \
  "$(missing_env "GATE_SYNC_TEST_SET GATE_SYNC_TEST_UNSET")"

# The one that matters: an Actions secret that does not exist arrives as an
# empty string, so "set but empty" has to count as missing or a run with no
# token reaches gh as "no credentials" instead of being named here.
assert_eq "an empty variable counts as missing, because that is how an absent secret arrives" \
  "GATE_SYNC_TEST_EMPTY" \
  "$(missing_env "GATE_SYNC_TEST_SET GATE_SYNC_TEST_EMPTY")"

assert_eq "every missing name is listed, not just the first" \
  "$(printf 'GATE_SYNC_TEST_EMPTY\nGATE_SYNC_TEST_UNSET')" \
  "$(missing_env "GATE_SYNC_TEST_EMPTY GATE_SYNC_TEST_SET GATE_SYNC_TEST_UNSET")"

unset GATE_SYNC_TEST_SET GATE_SYNC_TEST_EMPTY

# --- parse_consumers ---

assert_parse_fails() {
  local desc="$1" text="$2" expected_substring="$3" source_repo="${4:-inactdev/inspector}"
  local out rc=0
  out=$(parse_consumers "$text" "$source_repo") || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "FAIL: $desc - returned success instead of failing closed (parsed: \"$out\")"
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

assert_eq "a plain list parses to one owner/repo per line" \
  "$(printf 'inactdev/dotfiles\ninactdev/other')" \
  "$(parse_consumers "$(printf 'inactdev/dotfiles\ninactdev/other')" "inactdev/inspector")"

assert_eq "comments, blank lines and stray whitespace are ignored" \
  "$(printf 'inactdev/dotfiles\ninactdev/other')" \
  "$(parse_consumers "$(printf '# who runs the gate\n\n  inactdev/dotfiles  \n\n   # another\ninactdev/other\t\n')" "inactdev/inspector")"

assert_eq "a trailing comment on an entry is ignored" \
  "inactdev/dotfiles" \
  "$(parse_consumers "inactdev/dotfiles # the first consumer" "inactdev/inspector")"

assert_eq "a repository whose name starts with a dot is allowed (GitHub's own .github repo)" \
  "inactdev/.github" \
  "$(parse_consumers "inactdev/.github" "inactdev/inspector")"

# Every one of these fails the WHOLE list, not just its own line: this file
# decides where a token with write access to other repositories is pointed,
# and syncing the half of an ambiguous list that happened to parse is worse
# than refusing all of it.
assert_parse_fails "a line that is not an owner/repo pair fails closed, naming it" \
  "$(printf 'inactdev/dotfiles\nnot-a-repo')" \
  'line 2 is "not-a-repo"'

assert_parse_fails "an owner/repo/extra path fails closed" \
  "inactdev/dotfiles/deep" \
  'line 1 is "inactdev/dotfiles/deep"'

assert_parse_fails "a URL instead of a repository fails closed" \
  "https://github.com/inactdev/dotfiles" \
  "is not an \"owner/repo\" pair"

assert_parse_fails "a repository named twice fails closed" \
  "$(printf 'inactdev/dotfiles\ninactdev/dotfiles')" \
  'line 2 names "inactdev/dotfiles" a second time'

assert_parse_fails "the same repository in different case is still a duplicate" \
  "$(printf 'inactdev/dotfiles\nInactDev/DotFiles')" \
  "a second time"

assert_parse_fails "this repository naming itself fails closed" \
  "$(printf 'inactdev/dotfiles\ninactdev/inspector')" \
  "which is this repository itself"

assert_parse_fails "self-reference is caught regardless of case" \
  "InactDev/Inspector" \
  "which is this repository itself"

assert_parse_fails "a list with no repositories at all fails closed" \
  "$(printf '# nobody yet\n\n')" \
  "lists no repositories at all"

assert_parse_fails "an empty file fails closed" \
  "" \
  "lists no repositories at all"

# --- contents_base64_from_response ---
#
# Fixtures are shaped like a real `gh api --include` response: status line,
# headers, blank line, then the Contents API's JSON body.

contents_response() {
  local status="$1" body="$2"
  printf 'HTTP/2.0 %s\r\nContent-Type: application/json; charset=utf-8\r\n\r\n%s\n' "$status" "$body"
}

file_response() {
  local file_content="$1"
  contents_response "200 OK" \
    "$(jq -nc --arg c "$(printf '%s' "$file_content" | base64)" '{type: "file", content: $c, encoding: "base64"}')"
}

# The same, for a real file's exact bytes - including the trailing newline
# that $(...) would strip if the content went through a variable first.
file_response_from_file() {
  local encoded
  encoded=$(base64 < "$1")
  contents_response "200 OK" \
    "$(jq -nc --arg c "$encoded" '{type: "file", content: $c, encoding: "base64"}')"
}

assert_decode_fails() {
  local desc="$1" raw="$2" expected_substring="$3" gh_error="${4:-}"
  local out rc=0
  out=$(contents_base64_from_response "$raw" "$gh_error") || rc=$?
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

assert_eq "a 200 response decodes to the file's content, as base64" \
  "$(printf '%s' 'name: inspector-gate' | base64 | tr -d '\n')" \
  "$(contents_base64_from_response "$(file_response 'name: inspector-gate')")"

# GitHub wraps the base64 it hands back across several lines, so those breaks
# have to come out before anything is compared - otherwise every consumer
# looks different from every local file and the sync rewrites files that
# already match. ("bmFtZTog" + "aW5zcGVjdG9y" is "name: " + "inspector".)
assert_eq "the line breaks GitHub wraps its base64 with are stripped" \
  "$(printf '%s' 'name: inspector' | base64 | tr -d '\n')" \
  "$(contents_base64_from_response "$(contents_response "200 OK" \
    "$(jq -nc '{type: "file", content: "bmFtZTog\naW5zcGVjdG9y\n", encoding: "base64"}')")")"

# The comparison is done on the encodings, not on the decoded text, so that a
# difference of nothing but trailing newlines cannot read as "identical" -
# which is what comparing `$(<file)` against decoded content would do. This
# asserts the two sides line up on a real file, trailing newline and all.
assert_eq "a local file encodes to exactly what the API returns for the same bytes" \
  "$(file_base64 "$WORKFLOW")" \
  "$(contents_base64_from_response "$(file_response_from_file "$WORKFLOW")")"

# A 404 is the one safe absence - a consumer that does not carry the gate yet
# is precisely who this sync exists for - so it reports "no content" and
# succeeds, rather than failing the consumer.
absent_rc=0
absent_out=$(contents_base64_from_response "$(contents_response "404 Not Found" '{"message":"Not Found"}')") || absent_rc=$?
assert_eq "a 404 succeeds (the file is simply not there yet)" "0" "$absent_rc"
assert_eq "a 404 reports no content" "" "$absent_out"

assert_decode_fails "a rate limit fails closed, naming the status" \
  "$(contents_response "403 Forbidden" '{"message":"API rate limit exceeded"}')" \
  "HTTP 403"

assert_decode_fails "a server error fails closed, naming the status" \
  "$(contents_response "502 Bad Gateway" '{"message":"Server Error"}')" \
  "HTTP 502"

assert_decode_fails "no response at all (a network failure) fails closed" \
  "" \
  "no HTTP response at all"

assert_decode_fails "no response at all names gh's own reason when the caller has it" \
  "" \
  "no HTTP response at all: dial tcp: lookup api.github.com: no such host" \
  "dial tcp: lookup api.github.com: no such host"

assert_decode_fails "a reply that is not an HTTP response fails closed" \
  "gh: could not resolve host" \
  "did not come back as an HTTP response"

assert_decode_fails "a 200 whose body is not JSON says so" \
  "$(contents_response "200 OK" '{"content": truncated mid-resp')" \
  "not readable JSON"

# A directory at that path comes back as an ARRAY. Left unhandled it would
# read as "no content", therefore "different", therefore "overwrite it".
assert_decode_fails "a directory at that path fails closed rather than reading as empty" \
  "$(contents_response "200 OK" '[{"name":"inspector-gate.sh","type":"file"}]')" \
  "did not come back as a single file"

assert_decode_fails "a symlink at that path fails closed" \
  "$(contents_response "200 OK" '{"type":"symlink","target":"../elsewhere"}')" \
  "did not come back as a single file"

assert_decode_fails "a submodule at that path fails closed" \
  "$(contents_response "200 OK" '{"type":"submodule","submodule_git_url":"git://example/x"}')" \
  "did not come back as a single file"

# --- file_mode ---
#
# inspector-gate.sh is executable here and has to arrive executable at a
# consumer, or a project that runs it as a file rather than sourcing it gets a
# permission error instead of a gate.

assert_eq "the gate script syncs as an executable blob" \
  "100755" \
  "$(file_mode .github/scripts/inspector-gate.sh)"

assert_eq "the gate workflow syncs as a plain blob" \
  "100644" \
  "$(file_mode .github/workflows/inspector-gate.yml)"

# A path with no declared mode fails rather than defaulting to something
# plausible - 100644 on the gate script would be a consumer that cannot run
# its own gate.
mode_rc=0
file_mode ".github/scripts/not-synced.sh" > /dev/null || mode_rc=$?
assert_eq "an undeclared path has no mode, and says so instead of guessing" "1" "$mode_rc"

while IFS= read -r synced_path; do
  [ -n "$synced_path" ] || continue
  declared_rc=0
  file_mode "$synced_path" > /dev/null || declared_rc=$?
  assert_eq "$synced_path has a declared file mode" "0" "$declared_rc"
done <<< "$SYNCED_PATHS"

# The declared modes are held against what git actually records for those
# files here, which is the fact they are meant to carry. This is the one check
# in this file that needs git and a real checkout; where that is not available
# it says so rather than passing quietly, the same tolerance the Go tests
# extend to a missing container runtime.
if command -v git > /dev/null 2>&1 && git rev-parse --git-dir > /dev/null 2>&1; then
  while IFS= read -r synced_path; do
    [ -n "$synced_path" ] || continue
    git_mode=$(git ls-files -s -- "$synced_path" | awk 'NR == 1 { print $1 }')
    assert_eq "the declared mode for $synced_path is the one git records" \
      "$git_mode" "$(file_mode "$synced_path")"
  done <<< "$SYNCED_PATHS"
else
  echo "skipped: the declared modes could not be checked against git (no git, or not a checkout)"
fi

# --- the commit and pull request text ---
#
# The whole point of the sync is that a consumer can tell which commit of
# which repository its copy came from, so every piece of text it writes names
# that commit.

fixture_sha="0123456789abcdef0123456789abcdef01234567"
fixture_url="https://github.com/inactdev/inspector/commit/$fixture_sha"
fixture_message=$(commit_message "inactdev/inspector" "$fixture_sha" "$fixture_url")
fixture_title=$(pr_title "inactdev/inspector" "$fixture_sha")
fixture_body=$(pr_body "inactdev/inspector" "$fixture_sha" "$fixture_url")

assert_eq "a sha is abbreviated to twelve characters for human-facing text" \
  "0123456789ab" \
  "$(short_sha "$fixture_sha")"

assert_contains "the commit message names the source repository and commit" \
  "$fixture_message" "inactdev/inspector@0123456789ab"
assert_contains "the commit message names the full sha, not only the short one" \
  "$fixture_message" "$fixture_sha"
assert_contains "the commit message links the source commit" \
  "$fixture_message" "$fixture_url"

assert_contains "the pull request title names the source repository and commit" \
  "$fixture_title" "inactdev/inspector@0123456789ab"

assert_contains "the pull request body names the full source commit" \
  "$fixture_body" "$fixture_sha"
assert_contains "the pull request body links the source commit" \
  "$fixture_body" "$fixture_url"
assert_contains "the pull request body names the branch it maintains" \
  "$fixture_body" "$SYNC_BRANCH"
assert_contains "the pull request body says the consumer merges, not this sync" \
  "$fixture_body" "Nothing here merges this pull request"

while IFS= read -r synced_path; do
  [ -n "$synced_path" ] || continue
  assert_contains "the commit message lists $synced_path" "$fixture_message" "$synced_path"
  assert_contains "the pull request body lists $synced_path" "$fixture_body" "$synced_path"
done <<< "$SYNCED_PATHS"

# --- what is actually being synced ---

while IFS= read -r synced_path; do
  [ -n "$synced_path" ] || continue
  if [ -f "$synced_path" ]; then
    echo "ok: $synced_path exists to be synced"
  else
    echo "FAIL: SYNCED_PATHS names $synced_path, which does not exist in this repository"
    failures=$((failures + 1))
  fi
done <<< "$SYNCED_PATHS"

if [ -f "$CONSUMERS_FILE" ]; then
  echo "ok: $CONSUMERS_FILE exists"
else
  echo "FAIL: CONSUMERS_FILE names $CONSUMERS_FILE, which does not exist in this repository"
  failures=$((failures + 1))
fi

# The committed list is held to the same parser the sync uses, so a typo in it
# fails here rather than in a run that has already started writing to one
# consumer and not another.
committed_consumers_rc=0
committed_consumers=$(parse_consumers "$(<"$CONSUMERS_FILE")" "inactdev/inspector") || committed_consumers_rc=$?
assert_eq "the committed consumer list parses" "0" "$committed_consumers_rc"
assert_contains "the committed consumer list names inactdev/dotfiles" \
  "$committed_consumers" "inactdev/dotfiles"

# --- the workflow's own facts, read from its source ---
#
# Everything below reads .github/workflows/gate-sync.yml as text. Full-line
# comments are stripped first: this file's own prose mentions the triggers and
# tokens it is checking for the absence of, and a test that matched those
# would pass or fail on the comments rather than on the workflow.

workflow_body=$(awk '!/^[[:space:]]*#/' "$WORKFLOW")

# yaml_list_items prints the "- item" entries under a block-style key. It is a
# deliberately small reader, not a YAML parser: it expects the key alone on
# its line and its items as a block list below it, which is how this workflow
# is written. If that ever changes, this reader is the thing to change with
# it - silently reading nothing would turn the path-filter check below into a
# test that passes on an empty list.
yaml_list_items() {
  local file="$1" key="$2"
  awk -v key="$key" '
    $0 ~ "^[[:space:]]*" key ":[[:space:]]*$" { collecting = 1; next }
    collecting && /^[[:space:]]*#/ { next }
    collecting && /^[[:space:]]*-[[:space:]]/ {
      sub(/^[[:space:]]*-[[:space:]]+/, "")
      gsub(/"/, "")
      sub(/[[:space:]]+$/, "")
      print
      next
    }
    collecting { collecting = 0 }
  ' "$file"
}

assert_contains "the workflow is named gate-sync" "$workflow_body" "name: gate-sync"

# What wakes it up. A push to main is the only automatic trigger: the files
# being copied into other people's repositories must be the merged ones, so
# nothing here may fire on a pull request, and pull_request_target - which is
# what the gate itself runs on - would hand a write-scoped token to code that
# has not been reviewed.
assert_contains "it runs on push" "$workflow_body" "  push:"
assert_contains "only pushes to main trigger it" "$workflow_body" "branches: [main]"
assert_not_contains "no pull request event may trigger it" "$workflow_body" "pull_request"
assert_not_contains "it is not on a schedule either - a change to the gate is the trigger" \
  "$workflow_body" "schedule:"
assert_contains "it can be re-run by hand after a consumer's failure is fixed" \
  "$workflow_body" "workflow_dispatch:"

# The path filter and the script's own lists must agree. A synced file missing
# from `paths:` is a sync that never runs for the one change it exists for;
# the consumer list missing from it is a newly added consumer that waits for
# an unrelated gate change before it receives anything.
expected_trigger_paths="$SYNCED_PATHS
$CONSUMERS_FILE"
actual_trigger_paths=$(yaml_list_items "$WORKFLOW" "paths")

expected_path_count=0
while IFS= read -r expected_path; do
  [ -n "$expected_path" ] || continue
  expected_path_count=$((expected_path_count + 1))
  assert_contains "the workflow wakes on changes to $expected_path" \
    "$actual_trigger_paths" "$expected_path"
done <<< "$expected_trigger_paths"

actual_path_count=0
while IFS= read -r actual_path; do
  [ -n "$actual_path" ] || continue
  actual_path_count=$((actual_path_count + 1))
done <<< "$actual_trigger_paths"
assert_eq "the workflow's path filter is exactly the synced files plus the consumer list" \
  "$expected_path_count" "$actual_path_count"

# Which credentials it runs with. The default GITHUB_TOKEN cannot reach
# another repository at all, so naming it here would be a sync that fails on
# every consumer; GATE_SYNC_TOKEN is the repository secret scoped to them.
assert_contains "it authenticates with the GATE_SYNC_TOKEN secret" \
  "$workflow_body" 'GH_TOKEN: ${{ secrets.GATE_SYNC_TOKEN }}'
assert_not_contains "it does not fall back to the default GITHUB_TOKEN secret" \
  "$workflow_body" "secrets.GITHUB_TOKEN"
assert_not_contains "it does not fall back to the default github.token" \
  "$workflow_body" "github.token"

# What it may do to THIS repository: read it. Nothing it does is a write here.
assert_contains "it asks for read access to this repository" "$workflow_body" "contents: read"
assert_not_contains "it asks for no write permission at all" "$workflow_body" ": write"

assert_contains "two pushes cannot race to move the same consumer branch" \
  "$workflow_body" "group: gate-sync"
assert_not_contains "a half-finished sync is never cancelled mid-run" \
  "$workflow_body" "cancel-in-progress: true"

# It checks this repository out - which the gate itself must never do - and
# runs the sync script from it. Both are facts worth pinning: the script reads
# the synced files from disk, so without a checkout there is nothing to read.
assert_contains "it checks this repository out, so the synced files are on disk" \
  "$workflow_body" "actions/checkout"
assert_contains "it runs the sync script" "$workflow_body" "run: .github/scripts/gate-sync.sh"

# Every environment variable the script insists on has to be supplied by the
# workflow. Without this, adding one to REQUIRED_ENV and forgetting the env
# block would only ever show up as a failed run.
while IFS= read -r required_name; do
  [ -n "$required_name" ] || continue
  assert_contains "the workflow supplies $required_name" "$workflow_body" "$required_name:"
done <<< "$(printf '%s\n' $REQUIRED_ENV)"

# The gate's own two files are the thing being synced, and the gate workflow
# reads its script from this exact path on its base branch. If the gate ever
# moves one of them, SYNCED_PATHS has to move with it - and this is where
# that is noticed.
assert_contains "the gate workflow reads the script from the path that gets synced" \
  "$(<.github/workflows/inspector-gate.yml)" "contents/.github/scripts/inspector-gate.sh"

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "all gate-sync tests passed"
