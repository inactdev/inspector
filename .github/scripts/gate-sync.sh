#!/usr/bin/env bash
# gate-sync. inspector-gate is TWO files - .github/workflows/inspector-gate.yml
# and .github/scripts/inspector-gate.sh - and every repository that runs the
# gate keeps its own copy of both. The workflow deliberately never checks the
# repository out (SPEC.md section 8), so it reads the script from its own base
# branch through the GitHub API: a consumer carrying a stale copy of either
# file, or only one of them, is judged by a gate that is not the one this
# repository ships. Nothing kept those copies in step.
#
# This script is that missing half, and it is deliberately the same shape as
# Fabrica's consumer-contract sync (the opposite number of
# fabrica-contract-check.sh, which is how the other direction of that pair is
# enforced here): a push to main that changes either gate file opens or
# updates ONE pull request, on ONE fixed branch, on every repository named in
# .github/gate-consumers.txt, carrying both files verbatim and naming the
# inspector commit they came from.
#
# It never merges anything. Merging stays the consumer's own act, the same way
# a green gate here means "ready for a verdict", not "merged" (SPEC.md
# section 10).
#
# SAFETY NOTES, read before editing this file:
#
#   - The token this runs with (GATE_SYNC_TOKEN) has WRITE access to other
#     people's repositories. Two things bound where it can be pointed: the
#     consumer list is read from this repository's own committed file, never
#     from an input, and that file lives under .github/**, which
#     inspector-gate.sh's ALWAYS_PROTECTED floor covers - so changing it needs
#     the owner's eyes like any other gate file.
#   - Every GitHub error fails CLOSED. A consumer whose state could not be
#     read with certainty is never written to and never reported as synced.
#   - One consumer's failure does not stop the others: each is attempted, its
#     reason recorded, and the run fails at the end naming every one that
#     failed. One repository's revoked access must not leave the rest of them
#     carrying a stale gate.
#   - Nothing from a consumer repository is ever cloned, checked out or
#     executed. The commit is built entirely out of Git Database API calls
#     (blob, tree, commit, ref), so no git process ever receives this token
#     and no consumer content ever reaches this runner as code.
#   - The workflow that calls this DOES check this repository out, which
#     inspector-gate.yml must never do. The difference is the trigger, not a
#     change of policy: gate-sync runs on `push` to main, so the code it runs
#     is already merged and already past the gate.
#
# This file has two callers:
#   - the gate-sync workflow step, which executes it directly; the guard at
#     the bottom then calls main() to do the real GitHub API work
#   - gate-sync_test.sh, which sources this file instead (so the guard does
#     not fire) and calls the pure functions below directly, with fixture
#     data, never touching the network. main()'s API orchestration is only
#     exercisable by a real Actions run - the same honest limitation
#     inspector-gate.sh and fabrica-contract-check.sh document for themselves.
set -euo pipefail

# The files that are synced, verbatim, to every consumer - at exactly these
# paths, because inspector-gate.yml fetches the script by this path from its
# own base branch and a copy anywhere else is not the one that runs.
# gate-sync_test.sh checks this list against the workflow's own `paths:`
# trigger filter, so the two cannot drift into a sync that never wakes up.
SYNCED_PATHS=".github/workflows/inspector-gate.yml
.github/scripts/inspector-gate.sh"

# The git file mode each of those goes up with, one "path=mode" per line. See
# file_mode below for why these are declared here instead of read off the
# filesystem. Every entry in SYNCED_PATHS needs one, and a sync refuses to
# write a file whose mode is not declared rather than guessing at 100644.
SYNCED_MODES=".github/workflows/inspector-gate.yml=100644
.github/scripts/inspector-gate.sh=100755"

# The committed list of repositories to sync to. See its own header for the
# format; parse_consumers below is what enforces it.
CONSUMERS_FILE=".github/gate-consumers.txt"

# The one fixed branch every consumer's sync pull request lives on. Fixed, so
# repeated syncs update one pull request instead of opening a new one per
# inspector commit.
SYNC_BRANCH="inspector-gate-sync"

# Everything main() needs from the workflow. Named here rather than checked
# ad hoc so gate-sync_test.sh can prove the workflow actually supplies each
# one - a missing env var would otherwise only ever show up as a failed run.
REQUIRED_ENV="GH_TOKEN SOURCE_REPO SOURCE_SHA SOURCE_COMMIT_URL"

# --- pure functions (unit tested by gate-sync_test.sh) ---

# api_failure_reason turns a failed `gh api` call's captured stderr into one
# annotation-safe line - a raw newline ends a `::error::` annotation, so gh's
# lines are joined with "; " instead, and an empty capture still names the
# exit status. Same reasoning as inspector-gate.sh's gh_api_failure_reason and
# fabrica-contract-check.sh's copy, duplicated rather than sourced so this
# script does not depend on either of those files' unrelated state.
api_failure_reason() {
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

# missing_env prints, one per line, which of the named environment variables
# are unset or empty. Empty counts as missing on purpose: an Actions secret
# that does not exist arrives as an empty string, not as an unset variable, so
# a missing GATE_SYNC_TOKEN would otherwise reach `gh` as "no credentials"
# instead of being named here.
missing_env() {
  local names="$1" name
  for name in $names; do
    if [ -z "${!name:-}" ]; then
      printf '%s\n' "$name"
    fi
  done
}

# parse_consumers turns the consumer list file's text into one "owner/repo"
# per line, or fails closed with a single reason line on stdout.
#
# It fails the WHOLE list rather than skipping a bad entry. This file is the
# one input that decides where a token with write access to other people's
# repositories gets pointed; acting on the half of an ambiguous list that
# happened to parse is worse than refusing all of it and saying why.
#
# Rejected, each by name and line number: anything that is not an owner/repo
# pair, the same repository twice (GitHub names are case-insensitive, so the
# comparison is too), and the source repository naming itself - which would
# have this repository opening gate pull requests against its own gate.
parse_consumers() {
  local text="$1" source_repo="${2:-}"
  local line trimmed lower seen="" out="" count=0 lineno=0
  local source_key
  source_key=$(printf '%s' "$source_repo" | tr '[:upper:]' '[:lower:]')

  while IFS= read -r line || [ -n "$line" ]; do
    lineno=$((lineno + 1))
    # "#" cannot appear in an owner or repository name, so stripping from the
    # first one covers whole-line and trailing comments alike.
    trimmed="${line%%#*}"
    trimmed="${trimmed#"${trimmed%%[![:space:]]*}"}"
    trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
    [ -n "$trimmed" ] || continue

    if ! [[ "$trimmed" =~ ^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$ ]]; then
      echo "line $lineno is \"$trimmed\", which is not an \"owner/repo\" pair"
      return 1
    fi

    lower=$(printf '%s' "$trimmed" | tr '[:upper:]' '[:lower:]')
    if [ -n "$source_key" ] && [ "$lower" = "$source_key" ]; then
      echo "line $lineno names \"$trimmed\", which is this repository itself - the gate's own copy is the one being synced FROM, never to"
      return 1
    fi
    case "$seen" in
    *"|$lower|"*)
      echo "line $lineno names \"$trimmed\" a second time (repository names are case-insensitive)"
      return 1
      ;;
    esac
    seen="$seen|$lower|"
    out="$out$trimmed
"
    count=$((count + 1))
  done <<< "$text"

  if [ "$count" -eq 0 ]; then
    echo "it lists no repositories at all, so there is nothing to sync to"
    return 1
  fi
  printf '%s' "$out"
}

# contents_base64_from_response turns one GitHub Contents API `--include`
# response (status line, headers, blank line, JSON body) into that file's
# content as one unbroken base64 string, for comparison against a local file
# encoded the same way. Comparing the encodings rather than the decoded text
# keeps the comparison byte-exact: `$(...)` would strip trailing newlines off
# both sides and call two files the same when they are not.
#
# A 404 is the one safe absence - a consumer that does not have the gate yet
# is exactly who this sync is for - and prints empty content with a zero
# return. Every other way this read can go wrong fails closed with one reason
# line, because "could not read it" must never be mistaken for "it is already
# up to date" and skip the consumer.
#
# The first argument is the complete response, or "" for no response at all;
# the second is gh's own stderr reason, already run through
# api_failure_reason, named when there is no response to explain why.
contents_base64_from_response() {
  local raw="$1" gh_error="${2:-}"
  local proto http_status rest body encoded

  if [ -z "$raw" ]; then
    if [ -n "$gh_error" ]; then
      echo "the request produced no HTTP response at all: $gh_error"
    else
      echo "the request produced no HTTP response at all (a network failure, or gh could not reach GitHub)"
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
  if [ "$http_status" = "404" ]; then
    # Absent, which is a real answer: the consumer does not carry this file.
    return 0
  fi
  if [ "$http_status" != "200" ]; then
    echo "the GitHub Contents API returned HTTP $http_status"
    return 1
  fi

  body=$(awk 'in_body { print } /^\r?$/ { in_body = 1 }' <<< "$raw")

  # A directory comes back as an ARRAY, not an object with .content, and a
  # submodule or symlink as an object of a different type. None of those is a
  # file whose bytes can be compared, so none of them is allowed to read as
  # "no content, therefore different, therefore overwrite it".
  local kind
  if ! kind=$(jq -r 'type' <<< "$body" 2>/dev/null); then
    echo "its response body was not readable JSON, so the file's content could not be found in it"
    return 1
  fi
  if [ "$kind" != "object" ]; then
    echo "it did not come back as a single file (the API returned a JSON $kind rather than a file object - a directory at that path reads exactly this way)"
    return 1
  fi
  if ! encoded=$(jq -r 'if .type == "file" then (.content // "") else "not-a-file:" + (.type // "unknown") end' <<< "$body" 2>/dev/null); then
    echo "its response body was not readable JSON, so the file's content could not be found in it"
    return 1
  fi
  case "$encoded" in
  not-a-file:*)
    echo "it did not come back as a single file (the API called it a \"${encoded#not-a-file:}\")"
    return 1
    ;;
  esac
  printf '%s' "$encoded" | tr -d '\n'
}

# file_mode prints the git file mode one synced path goes up with, and fails
# when that path has no declared mode. inspector-gate.sh is executable in this
# repository and has to stay executable in a consumer's, or a project that
# runs it as a file rather than sourcing it gets a permission error instead of
# a gate.
#
# The modes are DECLARED, in SYNCED_MODES above, rather than read off this
# checkout with `[ -x ]`. A file mode is one of the two things being copied
# into somebody else's repository, and the filesystem's answer about it is not
# always the repository's: a share that reports everything as executable (some
# container and network mounts do exactly this) would quietly push a mode
# nobody chose. Declared, the mode sits next to the path where a reviewer sees
# both, cannot be bent by where the sync happens to run, and gate-sync_test.sh
# holds it against what git actually records here.
file_mode() {
  local path="$1" entry
  while IFS= read -r entry; do
    [ -n "$entry" ] || continue
    if [ "${entry%%=*}" = "$path" ]; then
      printf '%s' "${entry#*=}"
      return 0
    fi
  done <<< "$SYNCED_MODES"
  return 1
}

# short_sha abbreviates a commit for human-facing text. The full sha is always
# named too - this is only ever the shorter half of a line that carries both.
short_sha() {
  printf '%s' "${1:0:12}"
}

# synced_path_bullets lists the synced files for human-facing text.
synced_path_bullets() {
  local path
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    printf -- '- %s\n' "$path"
  done <<< "$SYNCED_PATHS"
}

# commit_message, pr_title and pr_body all name the inspector commit the files
# came from. That is the point of the whole sync: a consumer looking at its
# own copy can say which commit of which repository it is, without asking
# anyone.
commit_message() {
  local source_repo="$1" sha="$2" commit_url="$3"
  printf 'Sync inspector-gate from %s@%s\n\n' "$source_repo" "$(short_sha "$sha")"
  printf 'Carries these files verbatim from %s, as of commit %s:\n\n' "$source_repo" "$sha"
  synced_path_bullets
  printf '\nSource commit: %s\n' "$commit_url"
}

pr_title() {
  local source_repo="$1" sha="$2"
  printf 'Sync inspector-gate from %s@%s' "$source_repo" "$(short_sha "$sha")"
}

pr_body() {
  local source_repo="$1" sha="$2" commit_url="$3"
  printf 'This pull request carries inspector-gate'"'"'s two files verbatim from `%s`, as of commit `%s`:\n\n' \
    "$source_repo" "$sha"
  synced_path_bullets
  printf '\nSource commit: %s\n' "$commit_url"
  printf '\nBoth files belong together. The workflow never checks this repository out, so it reads `.github/scripts/inspector-gate.sh` from the base branch through the GitHub API - with only one of the two in place, every run fails closed saying it could not load its own definition.\n'
  printf '\nThe `%s` branch is maintained by `%s`'"'"'s gate-sync workflow: anything pushed to it by hand is replaced by the next sync'"'"'s commit on top of it, so change the gate in `%s` instead.\n' \
    "$SYNC_BRANCH" "$source_repo" "$source_repo"
  printf '\nNothing here merges this pull request. That stays your call.\n'
}

# --- GitHub API plumbing (only exercisable inside Actions) ---

# gh_api_call runs one `gh api` call with gh's stderr captured to a file
# rather than left to reach the log raw, turned into one annotation-safe line
# by api_failure_reason. It puts the response in GH_API_OUT (still set on
# failure - a non-200 body is a real answer, not noise) and the reason in
# GH_API_ERROR, then returns gh's own exit status.
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
    GH_API_ERROR=$(api_failure_reason "$(<"$stderr_file")" "$rc")
  fi
  rm -f "$stderr_file"
  return "$rc"
}

# gh_api_input is gh_api_call for the calls that send a JSON body: the body
# arrives on stdin and goes to `gh api --input -`, which is how a request with
# nested structure (a tree's array of entries) is sent. Field flags (-f) would
# only ever build a flat object, and with --input they become query-string
# parameters instead, so the two are never mixed here.
gh_api_input() {
  local body="$1"
  shift
  local stderr_file rc=0
  GH_API_OUT=""
  GH_API_ERROR=""
  stderr_file=$(mktemp) || {
    GH_API_ERROR="a temporary file to capture gh's stderr could not be created"
    return 1
  }
  GH_API_OUT=$(printf '%s' "$body" | gh api "$@" --input - 2>"$stderr_file") || rc=$?
  if [ "$rc" -ne 0 ]; then
    GH_API_ERROR=$(api_failure_reason "$(<"$stderr_file")" "$rc")
  fi
  rm -f "$stderr_file"
  return "$rc"
}

# file_base64 encodes one local file as a single unbroken base64 line - the
# form the Contents API hands back (once its line breaks are stripped) and the
# form a blob is created from. One encoding for both jobs, so "already up to
# date" and "what gets written" can never disagree about the bytes.
file_base64() {
  base64 < "$1" | tr -d '\n'
}

# SYNC_ERROR carries one consumer's failure reason back to main(), which
# records it and moves on to the next consumer rather than aborting the run.
# SYNC_OUT carries the helpers' results: they hand their answer back through
# a variable rather than by printing it, because `x=$(helper)` would run the
# helper in a subshell and the SYNC_ERROR it set on failure would be
# discarded with that subshell - leaving the caller to report a failure with
# no reason attached. Same shape as GH_API_OUT/GH_API_ERROR above.
SYNC_ERROR=""
SYNC_OUT=""

# consumer_file_base64 reads one path's current content on one ref of one
# consumer into SYNC_OUT, as base64, empty when the file is simply not there.
# Failures set SYNC_ERROR and return non-zero.
#
# --include so the HTTP status line comes back with the body: only a 404 is
# the safe "they do not have this file yet" absence, and gh exits non-zero on
# every non-2xx including that one, so the exit status alone decides nothing -
# the response does. See contents_base64_from_response.
#
# ref is a branch NAME, so it goes in as a parameter gh percent-encodes
# (-X GET -f ref=), never interpolated into the query string: git allows "&",
# "#", "%" and "+" in a branch name, and a consumer whose default branch is
# "release&hotfix" would otherwise have a DIFFERENT ref's content compared
# here. Same reasoning as inspector-gate.sh's .inspector.json read.
consumer_file_base64() {
  local repo="$1" path="$2" ref="$3"
  local response error encoded
  SYNC_OUT=""
  gh_api_call -X GET --include "repos/$repo/contents/$path" -f ref="$ref" || true
  response="$GH_API_OUT"
  error="$GH_API_ERROR"
  if ! encoded=$(contents_base64_from_response "$response" "$error"); then
    SYNC_ERROR="its copy of $path on \"$ref\" could not be read: $encoded"
    return 1
  fi
  SYNC_OUT="$encoded"
}

# consumer_carries_files reports whether one ref of one consumer already has
# every synced file with exactly this repository's bytes. Returns 0 for "all
# present and identical", 1 for "at least one differs or is missing", and 2
# for "could not tell" with SYNC_ERROR set - the caller must treat that third
# case as a failure, never as a difference to write over.
consumer_carries_files() {
  local repo="$1" ref="$2"
  local path rc=0
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    consumer_file_base64 "$repo" "$path" "$ref" || return 2
    if [ "$SYNC_OUT" != "$(file_base64 "$path")" ]; then
      rc=1
    fi
  done <<< "$SYNCED_PATHS"
  return "$rc"
}

# sync_branch_head_sha puts the sha the sync branch points at on a consumer
# into SYNC_OUT, or leaves it empty when that branch does not exist yet.
#
# This reads the matching-refs endpoint rather than the single-reference one,
# which answers a missing ref with a 404 - the same status a repository this
# token cannot see returns. Keeping "the branch is not there yet", which is
# the normal first-sync state, distinguishable from "this read failed" is
# worth one slightly odd-looking call. matching-refs is a PREFIX match, so the
# exact ref name is selected out of the array rather than trusting the first
# entry: a consumer with an "inspector-gate-sync-2" branch must not be read as
# having this one. SYNC_BRANCH is this file's own constant, which is why it
# can be put straight into the path and into the jq filter.
sync_branch_head_sha() {
  local repo="$1"
  SYNC_OUT=""
  if ! gh_api_call "repos/$repo/git/matching-refs/heads/$SYNC_BRANCH" \
    --jq ".[] | select(.ref == \"refs/heads/$SYNC_BRANCH\") | .object.sha"; then
    SYNC_ERROR="its \"$SYNC_BRANCH\" branch could not be looked up: $GH_API_ERROR"
    return 1
  fi
  SYNC_OUT="$GH_API_OUT"
}

# default_branch_tip puts the sha a consumer's default branch points at into
# SYNC_OUT - the commit a first sync branches from.
#
# A branch NAME is a parameter here (-f sha=), never part of the path: git
# allows "&", "#", "%" and "+" in one, and a consumer whose default branch is
# "release&hotfix" would otherwise have a different ref's tip read as its own.
# That rules out the refs endpoints, whose ref is a path segment, so this
# reads the commit listing instead and takes its first entry.
default_branch_tip() {
  local repo="$1" branch="$2"
  SYNC_OUT=""
  if ! gh_api_call -X GET "repos/$repo/commits" -f sha="$branch" -f per_page=1 \
    --jq '.[0].sha // ""'; then
    SYNC_ERROR="the tip of its default branch \"$branch\" could not be read, so there is nothing to base a pull request on (an empty repository with no commits reads this way too): $GH_API_ERROR"
    return 1
  fi
  SYNC_OUT="$GH_API_OUT"
}

# sync_one_consumer brings one repository's gate copy up to date and leaves an
# open pull request naming the inspector commit it came from. Progress goes to
# stdout as it happens; a failure sets SYNC_ERROR and returns non-zero, and
# main() records it and carries on to the next consumer.
sync_one_consumer() {
  local repo="$1" source_repo="$2" sha="$3" commit_url="$4"
  SYNC_ERROR=""

  local owner="${repo%%/*}"

  # The repository read comes first for a second reason beyond needing the
  # default branch: it is where "this token cannot see this repository" is
  # caught, so every later 404 can be read as the state it describes rather
  # than as a missing permission.
  local default_branch
  if ! gh_api_call "repos/$repo" --jq '.default_branch'; then
    SYNC_ERROR="it could not be read at all: $GH_API_ERROR. Check that it exists and that GATE_SYNC_TOKEN is scoped to it with Contents and Pull requests write access"
    return 1
  fi
  default_branch="$GH_API_OUT"
  if [ -z "$default_branch" ]; then
    SYNC_ERROR="its default branch came back empty, so there is no branch to open a pull request against"
    return 1
  fi

  # Already adopted? Then there is nothing to open, and saying so is the whole
  # answer. Checked against the default branch, not the sync branch, so a
  # merged sync is recognised as done however it was merged.
  local rc=0
  consumer_carries_files "$repo" "$default_branch" || rc=$?
  case "$rc" in
  0)
    echo "$repo: already carries both gate files on $default_branch - nothing to do."
    return 0
    ;;
  2) return 1 ;;
  esac

  sync_branch_head_sha "$repo" || return 1
  local branch_sha="$SYNC_OUT"

  # The commit's parent is the sync branch's own tip when that branch exists,
  # so the ref update below is always a fast-forward and nothing anyone pushed
  # to the branch is destroyed - a hand edit is replaced by a commit on top of
  # it, visibly, in the pull request's own history. Rebasing onto the default
  # branch and force-updating was the alternative and was rejected for that
  # reason; the cost accepted instead is that a long-lived sync branch can sit
  # behind the consumer's default branch until it is merged.
  local parent="$branch_sha"
  if [ -z "$parent" ]; then
    default_branch_tip "$repo" "$default_branch" || return 1
    parent="$SYNC_OUT"
    if [ -z "$parent" ]; then
      SYNC_ERROR="its default branch \"$default_branch\" has no commits, so there is nothing to base a pull request on"
      return 1
    fi
  else
    # The branch is already there. If it already carries exactly these bytes,
    # writing another commit would only add churn, so the commit is skipped
    # and the pull request's text is brought up to date instead. This is what
    # makes re-running the whole sync after fixing one consumer's access a
    # no-op for the consumers that were already fine.
    rc=0
    consumer_carries_files "$repo" "$SYNC_BRANCH" || rc=$?
    case "$rc" in
    0)
      echo "$repo: $SYNC_BRANCH already carries both gate files - leaving its commits alone."
      ensure_pull_request "$repo" "$owner" "$default_branch" "$source_repo" "$sha" "$commit_url" || return 1
      return 0
      ;;
    2) return 1 ;;
    esac
  fi

  # Each file goes up as its own blob, base64-encoded, rather than as a tree
  # entry's inline `content` string. "Verbatim" then means verbatim, not
  # "verbatim as long as it is valid UTF-8 text" - these are shell and YAML
  # today, and the promise this sync makes about them should not quietly
  # depend on that.
  local path entries="[]" blob_sha body mode
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    if [ ! -f "$path" ]; then
      SYNC_ERROR="$path does not exist in this repository, so it cannot be synced"
      return 1
    fi
    if ! mode=$(file_mode "$path"); then
      SYNC_ERROR="$path has no declared file mode in SYNCED_MODES, and a mode is not something to guess at when writing into another repository"
      return 1
    fi
    body=$(jq -nc --arg content "$(file_base64 "$path")" '{content: $content, encoding: "base64"}')
    if ! gh_api_input "$body" --method POST "repos/$repo/git/blobs" --jq '.sha'; then
      SYNC_ERROR="a blob for $path could not be created in it: $GH_API_ERROR"
      return 1
    fi
    blob_sha="$GH_API_OUT"
    if [ -z "$blob_sha" ]; then
      SYNC_ERROR="a blob for $path came back from it without a sha"
      return 1
    fi
    entries=$(jq -c --arg path "$path" --arg mode "$mode" --arg sha "$blob_sha" \
      '. + [{path: $path, mode: $mode, type: "blob", sha: $sha}]' <<< "$entries")
  done <<< "$SYNCED_PATHS"

  # base_tree is the parent commit's tree, so the new tree is that commit plus
  # these two entries. Without it the new tree would consist of these two
  # files ALONE and the commit would read as deleting every other file in the
  # repository.
  if ! gh_api_call "repos/$repo/git/commits/$parent" --jq '.tree.sha'; then
    SYNC_ERROR="the commit it branches from ($parent) could not be read: $GH_API_ERROR"
    return 1
  fi
  local base_tree="$GH_API_OUT"
  if [ -z "$base_tree" ]; then
    SYNC_ERROR="the commit it branches from ($parent) came back without a tree"
    return 1
  fi

  body=$(jq -nc --arg base "$base_tree" --argjson entries "$entries" '{base_tree: $base, tree: $entries}')
  if ! gh_api_input "$body" --method POST "repos/$repo/git/trees" --jq '.sha'; then
    SYNC_ERROR="a tree carrying the gate files could not be created in it: $GH_API_ERROR"
    return 1
  fi
  local tree_sha="$GH_API_OUT"
  if [ -z "$tree_sha" ]; then
    SYNC_ERROR="the tree carrying the gate files came back without a sha"
    return 1
  fi

  body=$(jq -nc --arg message "$(commit_message "$source_repo" "$sha" "$commit_url")" \
    --arg tree "$tree_sha" --arg parent "$parent" \
    '{message: $message, tree: $tree, parents: [$parent]}')
  if ! gh_api_input "$body" --method POST "repos/$repo/git/commits" --jq '.sha'; then
    SYNC_ERROR="the sync commit could not be created in it: $GH_API_ERROR"
    return 1
  fi
  local new_commit="$GH_API_OUT"
  if [ -z "$new_commit" ]; then
    SYNC_ERROR="the sync commit came back without a sha"
    return 1
  fi

  if [ -z "$branch_sha" ]; then
    body=$(jq -nc --arg ref "refs/heads/$SYNC_BRANCH" --arg sha "$new_commit" '{ref: $ref, sha: $sha}')
    if ! gh_api_input "$body" --method POST "repos/$repo/git/refs" --silent; then
      SYNC_ERROR="its \"$SYNC_BRANCH\" branch could not be created: $GH_API_ERROR"
      return 1
    fi
    echo "$repo: created $SYNC_BRANCH at $(short_sha "$new_commit")."
  else
    # force is false, and said out loud rather than left to the default: the
    # parent is this branch's own tip, so this is a fast-forward or it is a
    # race with someone else's push - and a race must fail, not overwrite.
    body=$(jq -nc --arg sha "$new_commit" '{sha: $sha, force: false}')
    if ! gh_api_input "$body" --method PATCH "repos/$repo/git/refs/heads/$SYNC_BRANCH" --silent; then
      SYNC_ERROR="its \"$SYNC_BRANCH\" branch could not be moved to the new commit: $GH_API_ERROR"
      return 1
    fi
    echo "$repo: updated $SYNC_BRANCH to $(short_sha "$new_commit")."
  fi

  ensure_pull_request "$repo" "$owner" "$default_branch" "$source_repo" "$sha" "$commit_url" || return 1
}

# ensure_pull_request opens the consumer's sync pull request, or updates the
# open one's title and body so it names the inspector commit now on the
# branch. One pull request per consumer, on one fixed branch, for the life of
# the sync - never one per inspector commit.
ensure_pull_request() {
  local repo="$1" owner="$2" base="$3" source_repo="$4" sha="$5" commit_url="$6"
  local title body number payload

  title=$(pr_title "$source_repo" "$sha")
  body=$(pr_body "$source_repo" "$sha" "$commit_url")

  # head is "owner:branch" because that is the format the list endpoint
  # documents for filtering by head branch; base keeps a pull request opened
  # against some other base from being mistaken for this one.
  if ! gh_api_call -X GET "repos/$repo/pulls" \
    -f state=open -f head="$owner:$SYNC_BRANCH" -f base="$base" \
    --jq '.[0].number // ""'; then
    SYNC_ERROR="its open pull requests could not be listed, so whether one is already open for $SYNC_BRANCH is unknown: $GH_API_ERROR"
    return 1
  fi
  number="$GH_API_OUT"

  if [ -z "$number" ]; then
    payload=$(jq -nc --arg title "$title" --arg head "$SYNC_BRANCH" --arg base "$base" --arg body "$body" \
      '{title: $title, head: $head, base: $base, body: $body}')
    if ! gh_api_input "$payload" --method POST "repos/$repo/pulls" --jq '.number'; then
      SYNC_ERROR="a pull request for $SYNC_BRANCH could not be opened on it: $GH_API_ERROR"
      return 1
    fi
    echo "$repo: opened pull request #$GH_API_OUT."
    return 0
  fi

  payload=$(jq -nc --arg title "$title" --arg body "$body" '{title: $title, body: $body}')
  if ! gh_api_input "$payload" --method PATCH "repos/$repo/pulls/$number" --silent; then
    SYNC_ERROR="its open pull request #$number could not be updated to name this commit: $GH_API_ERROR"
    return 1
  fi
  echo "$repo: updated pull request #$number."
}

# main reads the consumer list, then attempts every consumer in it before
# deciding anything. A consumer that fails is recorded and the next one is
# attempted anyway - one repository's problem must not leave the others
# carrying a stale gate - and the run then fails naming each failure.
main() {
  cd "$(dirname "${BASH_SOURCE[0]}")/../.."

  local absent
  absent=$(missing_env "$REQUIRED_ENV")
  if [ -n "$absent" ]; then
    # Joined onto one line: a raw newline would end the ::error:: annotation
    # and leave the rest of the names out of it.
    absent=$(printf '%s\n' "$absent" |
      awk 'NF { out = (out == "" ? $0 : out ", " $0) } END { print out }')
    echo "::error::This sync cannot run - not set in its environment: $absent. GH_TOKEN comes from the GATE_SYNC_TOKEN repository secret, and a secret that does not exist arrives empty rather than unset, which is the likeliest cause; the rest come from the workflow's own env block. Nothing was synced."
    exit 1
  fi

  if [ ! -f "$CONSUMERS_FILE" ]; then
    echo "::error::$CONSUMERS_FILE does not exist, so there is no list of repositories to sync the gate to. Nothing was synced."
    exit 1
  fi

  local consumers
  if ! consumers=$(parse_consumers "$(<"$CONSUMERS_FILE")" "$SOURCE_REPO"); then
    echo "::error::$CONSUMERS_FILE could not be read as a list of repositories: $consumers. Failing closed rather than syncing the part of it that parsed - this file decides where a token with write access to other repositories is pointed. Nothing was synced."
    exit 1
  fi

  local path
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    if [ ! -f "$path" ]; then
      echo "::error::$path does not exist in this repository, so the gate's own files cannot be read to sync them. Nothing was synced."
      exit 1
    fi
    if ! file_mode "$path" > /dev/null; then
      echo "::error::$path has no declared file mode in SYNCED_MODES, and a mode is not something to guess at when writing into another repository. Nothing was synced."
      exit 1
    fi
  done <<< "$SYNCED_PATHS"

  local repo failures="" ok=0 failed=0
  echo "Syncing inspector-gate from $SOURCE_REPO@$(short_sha "$SOURCE_SHA") to:"
  while IFS= read -r repo; do
    [ -n "$repo" ] || continue
    echo "  $repo"
  done <<< "$consumers"

  # Every consumer is attempted before anything is decided. A failure here is
  # recorded and the loop carries on: one repository's revoked access, branch
  # protection or outage must not leave the others carrying a stale gate.
  while IFS= read -r repo; do
    [ -n "$repo" ] || continue
    if sync_one_consumer "$repo" "$SOURCE_REPO" "$SOURCE_SHA" "$SOURCE_COMMIT_URL"; then
      ok=$((ok + 1))
    else
      failed=$((failed + 1))
      failures="$failures$repo"$'\t'"$SYNC_ERROR"$'\n'
      # Named immediately as well as in the summary, so the failing
      # consumer's reason sits next to the work that was happening when it
      # failed rather than only at the end of a long log.
      echo "::error::$repo could not be synced: $SYNC_ERROR"
    fi
  done <<< "$consumers"

  if [ "$failed" -eq 0 ]; then
    echo "All $ok consumer(s) are up to date with $SOURCE_REPO@$(short_sha "$SOURCE_SHA")."
    return 0
  fi

  echo "::error::$failed of $((ok + failed)) consumer(s) could not be synced to $SOURCE_REPO@$(short_sha "$SOURCE_SHA"), each named below with its reason. Every consumer was attempted, so the ones not named here are up to date; re-running this workflow once a cause is fixed does nothing to those, and can be run again as many times as it takes."
  local line name reason
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    IFS=$'\t' read -r name reason <<< "$line"
    echo "  $name: $reason"
  done <<< "$failures"
  exit 1
}

# Run main() when this file is executed directly (the gate-sync workflow
# step), but not when it is sourced (gate-sync_test.sh, which calls the pure
# functions directly and never touches the network).
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
