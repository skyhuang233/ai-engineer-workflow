#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
source "$SCRIPT_DIR/github-afk-common.sh"

test_candidate_retries_transient_blocker_read() (
  export GH_AFK_RETRY_ATTEMPTS=3
  export GH_AFK_RETRY_BASE_DELAY_SECONDS=0
  export GH_AFK_RETRY_MAX_DELAY_SECONDS=0

  local calls
  local body
  local output
  local status

  calls="$(mktemp)"
  trap 'rm -f "$calls"' EXIT

  sleep() {
    :
  }

  gh() {
    printf 'call\n' >> "$calls"
    if [ "$(wc -l < "$calls" | tr -d ' ')" -eq 1 ]; then
      echo 'Post "https://api.github.com/graphql": net/http: TLS handshake timeout' >&2
      return 1
    fi
    printf 'CLOSED\n'
  }

  body=$'## What to build\n\nBuild it.\n\n## Acceptance criteria\n\n- [ ] It works.\n\n## Blocked by\n\n- #5\n'

  set +e
  output="$(gh_afk_check_issue_candidate "8" "Delivery ticket" "$body" 2>&1)"
  status="$?"
  set -e

  if [ "$status" -ne 0 ]; then
    printf '%s\n' "$output" >&2
    echo "expected candidate check to recover from one TLS timeout" >&2
    return 1
  fi

  if [ "$(wc -l < "$calls" | tr -d ' ')" -ne 2 ]; then
    echo "expected exactly two blocker reads" >&2
    return 1
  fi
)

test_label_lookup_retries_without_create() (
  export GH_AFK_RETRY_ATTEMPTS=3
  export GH_AFK_RETRY_BASE_DELAY_SECONDS=0
  export GH_AFK_RETRY_MAX_DELAY_SECONDS=0

  local list_calls
  local create_calls

  list_calls="$(mktemp)"
  create_calls="$(mktemp)"
  trap 'rm -f "$list_calls" "$create_calls"' EXIT

  sleep() {
    :
  }

  gh() {
    if [ "$1 $2" = "label list" ]; then
      printf 'call\n' >> "$list_calls"
      if [ "$(wc -l < "$list_calls" | tr -d ' ')" -eq 1 ]; then
        echo 'Post "https://api.github.com/graphql": net/http: TLS handshake timeout' >&2
        return 1
      fi
      printf 'needs-info\n'
      return 0
    fi

    if [ "$1 $2" = "label create" ]; then
      printf 'call\n' >> "$create_calls"
      echo 'label with name "needs-info" already exists' >&2
      return 1
    fi

    return 99
  }

  if ! gh_afk_ensure_label "needs-info" "D876E3" "Waiting on reporter"; then
    echo "expected label lookup to recover from one TLS timeout" >&2
    return 1
  fi

  if [ "$(wc -l < "$list_calls" | tr -d ' ')" -ne 2 ]; then
    echo "expected exactly two label-list calls" >&2
    return 1
  fi

  if [ -s "$create_calls" ]; then
    echo "label creation must not run after a transient list failure" >&2
    return 1
  fi
)

test_exhausted_transport_error_is_not_malformed() (
  export GH_AFK_RETRY_ATTEMPTS=2
  export GH_AFK_RETRY_BASE_DELAY_SECONDS=0
  export GH_AFK_RETRY_MAX_DELAY_SECONDS=0

  local body
  local output
  local status

  sleep() {
    :
  }

  gh() {
    echo 'Post "https://api.github.com/graphql": net/http: TLS handshake timeout' >&2
    return 1
  }

  body=$'## What to build\n\nBuild it.\n\n## Acceptance criteria\n\n- [ ] It works.\n\n## Blocked by\n\n- #5\n'

  set +e
  output="$(gh_afk_check_issue_candidate "8" "Delivery ticket" "$body" 2>&1)"
  status="$?"
  set -e

  if [ "$status" -ne 3 ]; then
    printf '%s\n' "$output" >&2
    echo "expected transport failure status 3, got $status" >&2
    return 1
  fi

  if grep -Fq "Malformed ready-for-agent issue" <<<"$output"; then
    printf '%s\n' "$output" >&2
    echo "transport failure must not be reported as malformed issue data" >&2
    return 1
  fi
)

test_non_transient_error_is_not_retried() (
  export GH_AFK_RETRY_ATTEMPTS=5
  export GH_AFK_RETRY_BASE_DELAY_SECONDS=0
  export GH_AFK_RETRY_MAX_DELAY_SECONDS=0

  local calls
  local output
  local status

  calls="$(mktemp)"
  trap 'rm -f "$calls"' EXIT

  sleep() {
    :
  }

  gh() {
    printf 'call\n' >> "$calls"
    echo 'HTTP 401: Bad credentials' >&2
    return 1
  }

  set +e
  output="$(gh_afk_retry_read gh issue view 5 --json state 2>&1)"
  status="$?"
  set -e

  if [ "$status" -eq 0 ]; then
    echo "expected non-transient error to fail" >&2
    return 1
  fi

  if [ "$(wc -l < "$calls" | tr -d ' ')" -ne 1 ]; then
    printf '%s\n' "$output" >&2
    echo "non-transient errors must not be retried" >&2
    return 1
  fi
)

failures=0

for test_case in \
  test_candidate_retries_transient_blocker_read \
  test_label_lookup_retries_without_create \
  test_exhausted_transport_error_is_not_malformed \
  test_non_transient_error_is_not_retried
do
  if "$test_case"; then
    echo "ok - $test_case"
  else
    echo "not ok - $test_case"
    failures=$((failures + 1))
  fi
done

exit "$failures"
