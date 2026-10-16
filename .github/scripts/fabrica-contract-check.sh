#!/usr/bin/env bash
# fabrica-contract-check (issue #109, "Inspector side"). Fabrica and
# inspector each keep their own copy of the consumer contract: Fabrica's
# is src/inspector/consumer-contract.json, read by its own run.test.ts;
# inspector's is cmd/inspector/testdata/fabrica-consumer-contract.json,
# read by TestFabricaConsumerContract. Nothing keeps two separately
# committed copies in step on its own, so this script is the half that
# runs here: it reads Fabrica's file from Fabrica's default branch
# through the GitHub Contents API (the same read inspector-gate.sh uses
# for .inspector.json) and fails, printing a diff, if inspector's copy no
# longer matches byte-for-byte. The only way inspector's copy is meant to
# change is a Fabrica-made pull request replacing it wholesale - see
# AGENTS.md.
#
# Unlike that .inspector.json read, a 404 here is never a safe absence:
# there is no state where Fabrica's file is missing and inspector's copy
# can still be trusted, so every non-200 response fails closed and is
# named, 404 included.
#
# This file has two callers:
#   - the CI workflow step, which executes it directly; the guard at the
#     bottom of this file then calls main() to do the real GitHub API work
#   - fabrica-contract-check_test.sh, which sources this file instead
#     (so the guard does not fire) and calls decode_contents_response
#     directly, with fixture data, never touching the network. main()'s
#     own API orchestration is exercised only by a real Actions run - the
#     same honest limitation inspector-gate.sh documents for itself.
set -euo pipefail

FABRICA_REPO="inactdev/fabrica"
FABRICA_CONTRACT_PATH="src/inspector/consumer-contract.json"
LOCAL_CONTRACT_PATH="cmd/inspector/testdata/fabrica-consumer-contract.json"

# api_failure_reason turns a failed `gh api` call's captured stderr into
# one annotation-safe line - a raw newline ends a `::error::` annotation,
# so gh's lines are joined with "; " instead, and an empty capture still
# names the exit status. Same reasoning as inspector-gate.sh's
# gh_api_failure_reason, duplicated rather than sourced so this script
# does not depend on that gate's unrelated state (ALWAYS_PROTECTED, its
# own main).
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

# decode_contents_response turns one GitHub Contents API `--include`
# response (status line, headers, blank line, base64 body) into the
# file's decoded content, or fails closed with one reason line printed to
# stdout and a non-zero return. The first argument is the complete
# response, or "" for no response at all; the second is gh's own stderr
# reason, already run through api_failure_reason, named when there is no
# response to explain why a network failure, DNS, or a gh crash happened.
decode_contents_response() {
  local raw="$1" gh_error="${2:-}"
  local proto http_status rest body encoded content

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
  if [ "$http_status" != "200" ]; then
    echo "the GitHub Contents API returned HTTP $http_status"
    return 1
  fi

  body=$(awk 'in_body { print } /^\r?$/ { in_body = 1 }' <<< "$raw")

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
  printf '%s' "$content"
}

# gh_api_call runs one `gh api` call with gh's stderr captured to a file
# rather than left to reach the log raw, turned into one annotation-safe
# line by api_failure_reason. It puts the response in GH_API_OUT (still
# set on failure - a non-200 body is a real answer, not noise) and the
# reason in GH_API_ERROR, then returns gh's own exit status. Part of
# main()'s orchestration, untested here for the same reason the rest of
# it is: see this file's header.
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
    GH_API_ERROR=$(api_failure_reason "$(cat "$stderr_file")" "$rc")
  fi
  rm -f "$stderr_file"
  return "$rc"
}

# main resolves Fabrica's default branch, reads its consumer contract at
# that branch's tip, and fails - printing a diff - if inspector's own
# copy no longer matches byte-for-byte. Only exercisable inside Actions;
# see this file's header.
main() {
  : "${GH_TOKEN:?}"
  cd "$(dirname "${BASH_SOURCE[0]}")/../.."

  local default_branch
  if ! gh_api_call "repos/$FABRICA_REPO" --jq '.default_branch'; then
    echo "::error::Could not read Fabrica's default branch from the GitHub API, so there is no branch to read its consumer contract from: $GH_API_ERROR. Failing closed rather than guessing - this is a problem reaching GitHub, not a verdict on the contract; re-run this check."
    exit 1
  fi
  default_branch="$GH_API_OUT"
  if [ -z "$default_branch" ]; then
    echo "::error::Fabrica's default branch came back empty from the GitHub API, so there is no branch to read its consumer contract from. Failing closed rather than guessing; re-run this check."
    exit 1
  fi

  gh_api_call -X GET --include "repos/$FABRICA_REPO/contents/$FABRICA_CONTRACT_PATH" -f ref="$default_branch" || true
  local fabrica_content
  if ! fabrica_content=$(decode_contents_response "$GH_API_OUT" "$GH_API_ERROR"); then
    echo "::error::Could not read Fabrica's consumer contract (repos/$FABRICA_REPO/contents/$FABRICA_CONTRACT_PATH @ $default_branch) from the GitHub API: $fabrica_content. Failing closed rather than passing unchecked - $LOCAL_CONTRACT_PATH can only be trusted once it is actually compared against Fabrica's copy; re-run this check, or confirm that file exists at that path on Fabrica's default branch."
    exit 1
  fi

  if [ ! -f "$LOCAL_CONTRACT_PATH" ]; then
    echo "::error::$LOCAL_CONTRACT_PATH does not exist, so it cannot be compared against Fabrica's consumer contract."
    exit 1
  fi
  local local_content
  local_content=$(cat "$LOCAL_CONTRACT_PATH")

  if [ "$local_content" = "$fabrica_content" ]; then
    echo "$LOCAL_CONTRACT_PATH matches Fabrica's consumer contract at $FABRICA_REPO@$default_branch."
    return 0
  fi

  echo "::error::$LOCAL_CONTRACT_PATH no longer matches Fabrica's consumer contract (repos/$FABRICA_REPO/contents/$FABRICA_CONTRACT_PATH @ $default_branch). The only way this file may change is a Fabrica-made pull request replacing it wholesale - see AGENTS.md. Diff ($LOCAL_CONTRACT_PATH vs Fabrica's):"
  diff -u <(printf '%s\n' "$local_content") <(printf '%s\n' "$fabrica_content") || true
  exit 1
}

# Run main() when this file is executed directly (the CI workflow step),
# but not when it is sourced (fabrica-contract-check_test.sh, which calls
# decode_contents_response and api_failure_reason directly without ever
# touching the network).
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
