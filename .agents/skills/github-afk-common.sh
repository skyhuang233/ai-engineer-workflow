#!/usr/bin/env bash
set -euo pipefail

gh_afk_require_tools() {
  local missing=0
  local cmd
  for cmd in git gh jq awk grep sort; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      echo "Required command not found on PATH: $cmd" >&2
      missing=1
    fi
  done
  if [ "$missing" -ne 0 ]; then
    return 1
  fi

  if gh auth status >/dev/null 2>&1; then
    return 0
  fi

  # GITHUB_TOKEN overrides credentials stored by gh. If only that override is
  # invalid, keep the AFK run scoped to the working saved credential.
  if [ -n "${GITHUB_TOKEN:-}" ] && (unset GITHUB_TOKEN; gh auth status) >/dev/null 2>&1; then
    echo "Ignoring invalid GITHUB_TOKEN; using the GitHub CLI saved credential." >&2
    unset GITHUB_TOKEN
    return 0
  fi

  gh auth status
}

gh_afk_is_transient_github_error() {
  local error_file="$1"

  grep -Eiq \
    'TLS handshake timeout|dial tcp|connectex:|connection reset by peer|connection timed out|i/o timeout|unexpected EOF|(^|: )EOF$|could not resolve host|temporary failure|HTTP (502|503|504)|Bad Gateway|Service Unavailable|Gateway Timeout|stream error' \
    "$error_file"
}

gh_afk_retry_read() {
  local attempts="${GH_AFK_RETRY_ATTEMPTS:-5}"
  local delay="${GH_AFK_RETRY_BASE_DELAY_SECONDS:-2}"
  local max_delay="${GH_AFK_RETRY_MAX_DELAY_SECONDS:-30}"
  local attempt
  local status
  local stdout_file
  local stderr_file

  if ! [[ "$attempts" =~ ^[1-9][0-9]*$ ]]; then
    attempts=5
  fi
  if ! [[ "$delay" =~ ^[0-9]+$ ]]; then
    delay=2
  fi
  if ! [[ "$max_delay" =~ ^[0-9]+$ ]]; then
    max_delay=30
  fi
  if [ "$delay" -gt "$max_delay" ]; then
    delay="$max_delay"
  fi

  stdout_file="$(mktemp)"
  stderr_file="$(mktemp)"

  for ((attempt=1; attempt<=attempts; attempt++)); do
    if "$@" >"$stdout_file" 2>"$stderr_file"; then
      cat "$stdout_file"
      cat "$stderr_file" >&2
      rm -f "$stdout_file" "$stderr_file"
      return 0
    else
      status="$?"
    fi

    if [ "$attempt" -ge "$attempts" ] \
      || ! gh_afk_is_transient_github_error "$stderr_file"; then
      cat "$stdout_file"
      cat "$stderr_file" >&2
      rm -f "$stdout_file" "$stderr_file"
      return "$status"
    fi

    cat "$stderr_file" >&2
    echo "Transient GitHub read failure; retrying in ${delay}s (attempt $attempt/$attempts)." >&2
    if [ "$delay" -gt 0 ]; then
      sleep "$delay"
    fi

    : >"$stdout_file"
    : >"$stderr_file"

    if [ "$delay" -lt "$max_delay" ]; then
      delay=$((delay * 2))
      if [ "$delay" -gt "$max_delay" ]; then
        delay="$max_delay"
      fi
    fi
  done

  rm -f "$stdout_file" "$stderr_file"
  return 1
}

gh_afk_ensure_label() {
  local name="$1"
  local color="$2"
  local description="$3"
  local labels
  local create_error_file
  local create_status

  if ! labels="$(gh_afk_retry_read gh label list --limit 1000 --json name --jq '.[].name')"; then
    echo "Cannot ensure label '$name': unable to list repository labels." >&2
    return 1
  fi

  if grep -Fxq "$name" <<<"$labels"; then
    return 0
  fi

  create_error_file="$(mktemp)"
  if gh label create "$name" --color "$color" --description "$description" \
    >/dev/null 2>"$create_error_file"; then
    rm -f "$create_error_file"
    return 0
  else
    create_status="$?"
  fi

  # A create request may have reached GitHub even when its response was lost.
  # Re-read before reporting failure so an existing label remains idempotent.
  if labels="$(gh_afk_retry_read gh label list --limit 1000 --json name --jq '.[].name')" \
    && grep -Fxq "$name" <<<"$labels"; then
    rm -f "$create_error_file"
    return 0
  fi

  cat "$create_error_file" >&2
  rm -f "$create_error_file"
  return "$create_status"
}

gh_afk_ensure_labels() {
  gh_afk_ensure_label "needs-triage" "D4C5F9" "Maintainer needs to evaluate this issue"
  gh_afk_ensure_label "needs-info" "D876E3" "Waiting on reporter for more information"
  gh_afk_ensure_label "ready-for-agent" "0E8A16" "Fully specified, ready for an AFK agent"
  gh_afk_ensure_label "ready-for-human" "1D76DB" "Requires human implementation"
  gh_afk_ensure_label "wontfix" "FFFFFF" "Will not be actioned"
  gh_afk_ensure_label "prd" "5319E7" "Parent planning issue, not directly executable AFK work"
  gh_afk_ensure_label "blocked" "D93F0B" "Blocked until a dependency or external condition changes"
  gh_afk_ensure_label "in-progress" "FBCA04" "Claimed by an AFK loop"
}

gh_afk_assert_clean_worktree() {
  local status
  status="$(git status --short)"
  if [ -n "$status" ]; then
    echo "Refusing to start an AFK issue with a dirty worktree." >&2
    echo "Commit, stash, or inspect these changes first:" >&2
    echo "$status" >&2
    return 1
  fi
}

gh_afk_assert_checkout() {
  local expected_branch="$1"
  local expected_root="$2"
  local actual_root
  local normalized_actual_root
  local normalized_expected_root
  local actual_branch

  if ! actual_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
    echo "Refusing to start AFK outside a git checkout." >&2
    return 1
  fi
  normalized_actual_root="$(gh_afk_normalize_checkout_path "$actual_root")"
  normalized_expected_root="$(gh_afk_normalize_checkout_path "$(cd "$expected_root" && pwd)")"

  if [ "$normalized_actual_root" != "$normalized_expected_root" ]; then
    echo "Refusing to start AFK from the wrong checkout root." >&2
    echo "Expected: $normalized_expected_root" >&2
    echo "Actual:   $normalized_actual_root" >&2
    return 1
  fi

  if ! actual_branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null)"; then
    echo "Refusing to start AFK because the current branch is unreadable." >&2
    return 1
  fi
  actual_branch="$(gh_afk_strip_cr "$actual_branch")"

  if [ "$actual_branch" != "$expected_branch" ]; then
    echo "Refusing to start AFK on branch '$actual_branch'." >&2
    echo "Expected branch: '$expected_branch'" >&2
    return 1
  fi
}

gh_afk_extract_section() {
  local heading="$1"
  local file="$2"
  awk -v name="$heading" '
    BEGIN { target = "^##[[:space:]]+" name "[[:space:]]*$"; in_section = 0 }
    $0 ~ target { in_section = 1; next }
    in_section && /^##[[:space:]]+/ { exit }
    in_section { print }
  ' "$file"
}

gh_afk_has_text() {
  grep -Eq '[^[:space:]]'
}

gh_afk_has_issue_contract_sections() {
  local file="$1"
  local what_to_build
  local acceptance
  local blocked_by

  what_to_build="$(gh_afk_extract_section "What to build" "$file")"
  acceptance="$(gh_afk_extract_section "Acceptance criteria" "$file")"
  blocked_by="$(gh_afk_extract_section "Blocked by" "$file")"

  printf '%s' "$what_to_build" | gh_afk_has_text \
    && printf '%s' "$acceptance" | gh_afk_has_text \
    && printf '%s' "$blocked_by" | gh_afk_has_text
}

gh_afk_structured_issue_comment_body() {
  local number="$1"

  gh_afk_retry_read gh issue view "$number" --json comments --jq '
    (.comments // [])
    | map(.body // "")
    | map(select(
        test("(^|\\n)##[[:space:]]+What to build[[:space:]]*(\\n|$)")
        and test("(^|\\n)##[[:space:]]+Acceptance criteria[[:space:]]*(\\n|$)")
        and test("(^|\\n)##[[:space:]]+Blocked by[[:space:]]*(\\n|$)")
      ))
    | last // ""
  '
}

gh_afk_resolve_issue_contract_body() {
  local number="$1"
  local body="$2"
  local body_file
  local fallback_body

  body_file="$(mktemp)"
  printf '%s\n' "$body" > "$body_file"

  if gh_afk_has_issue_contract_sections "$body_file"; then
    rm -f "$body_file"
    printf '%s' "$body"
    return 0
  fi

  rm -f "$body_file"

  if ! fallback_body="$(gh_afk_structured_issue_comment_body "$number")"; then
    return 1
  fi
  fallback_body="$(gh_afk_strip_cr "$fallback_body")"

  if ! printf '%s' "$fallback_body" | gh_afk_has_text; then
    return 1
  fi

  body_file="$(mktemp)"
  printf '%s\n' "$fallback_body" > "$body_file"

  if ! gh_afk_has_issue_contract_sections "$body_file"; then
    rm -f "$body_file"
    return 1
  fi

  rm -f "$body_file"
  printf '%s' "$fallback_body"
}

gh_afk_issue_refs_from_text() {
  local text="$1"
  printf '%s\n' "$text" \
    | grep -Eo '#[0-9]+|issues/[0-9]+' \
    | grep -Eo '[0-9]+' \
    | sort -nu \
    || true
}

gh_afk_strip_cr() {
  local value="${1:-}"
  printf '%s' "${value//$'\r'/}"
}

gh_afk_normalize_checkout_path() {
  local path
  local drive
  local rest

  path="$(gh_afk_strip_cr "${1:-}")"
  path="${path//\\//}"

  case "$path" in
    /[A-Za-z]/*)
      drive="${path:1:1}"
      rest="${path:3}"
      printf '%s' "${drive^^}:/$rest"
      ;;
    [A-Za-z]:/*)
      drive="${path:0:1}"
      printf '%s' "${drive^^}${path:1}"
      ;;
    *)
      printf '%s' "$path"
      ;;
  esac
}

gh_afk_to_bash_path() {
  local path
  local drive
  local rest

  path="$(gh_afk_strip_cr "${1:-}")"
  path="${path//\\//}"

  case "$path" in
    "~/"*)
      printf '%s/%s' "$HOME" "${path#"~/"}"
      ;;
    "~")
      printf '%s' "$HOME"
      ;;
    [A-Za-z]:/*)
      drive="${path:0:1}"
      rest="${path:3}"
      printf '/%s/%s' "${drive,,}" "$rest"
      ;;
    *)
      printf '%s' "$path"
      ;;
  esac
}

gh_afk_resolve_treehouse() {
  local candidate

  for candidate in treehouse.exe treehouse; do
    if command -v "$candidate" >/dev/null 2>&1; then
      command -v "$candidate"
      return 0
    fi
  done

  echo "Treehouse executable not found on PATH." >&2
  return 1
}

gh_afk_treehouse_worktree_for_holder() {
  local treehouse="$1"
  local holder="$2"
  local path

  path="$("$treehouse" status | awk -v holder="$holder" '
    {
      held = ""
      if (match($0, /\(held by [^)]+\)$/)) {
        held = substr($0, RSTART + 9, RLENGTH - 10)
      }
      if (held != holder) {
        next
      }

      path = $0
      sub(/^[[:space:]]*[0-9]+[[:space:]]+[[:alnum:]-]+[[:space:]]+/, "", path)
      sub(/[[:space:]]+\(held by [^)]+\)$/, "", path)
      print path
      exit
    }
  ')"
  path="$(gh_afk_strip_cr "$path")"
  if [ -n "$path" ]; then
    gh_afk_to_bash_path "$path"
  fi
}

gh_afk_acquire_treehouse_worktree() {
  local treehouse="$1"
  local holder="$2"
  local path

  if ! path="$("$treehouse" get --lease --lease-holder "$holder")"; then
    echo "Failed to acquire a Treehouse worktree for holder '$holder'." >&2
    return 1
  fi

  path="$(gh_afk_strip_cr "$path")"
  if [ -z "$path" ]; then
    echo "Treehouse returned an empty worktree path for holder '$holder'." >&2
    return 1
  fi

  gh_afk_to_bash_path "$path"
}

gh_afk_prepare_treehouse_branch() {
  local worktree_path="$1"
  local expected_branch="$2"
  local base_branch="$3"
  local current_branch
  local status

  if ! current_branch="$(git -C "$worktree_path" rev-parse --abbrev-ref HEAD 2>/dev/null)"; then
    echo "Cannot read the current branch for Treehouse worktree: $worktree_path" >&2
    return 1
  fi
  current_branch="$(gh_afk_strip_cr "$current_branch")"

  if [ "$current_branch" = "$expected_branch" ]; then
    return 0
  fi

  status="$(git -C "$worktree_path" status --short)"
  if [ -n "$status" ]; then
    echo "Refusing to switch the AFK Treehouse worktree with uncommitted changes." >&2
    echo "Worktree: $worktree_path" >&2
    echo "$status" >&2
    return 1
  fi

  if git -C "$worktree_path" show-ref --verify --quiet "refs/heads/$expected_branch"; then
    git -C "$worktree_path" switch "$expected_branch" >/dev/null
  else
    git -C "$worktree_path" switch -c "$expected_branch" "$base_branch" >/dev/null
  fi
}

gh_afk_enter_treehouse_worktree() {
  local holder="$1"
  local expected_branch="$2"
  local base_branch="$3"
  local script_rel="$4"
  shift 4

  local treehouse
  local worktree_path
  local current_branch
  local status

  if [ "${GH_AFK_TREEHOUSE_ACTIVE:-}" = "$holder" ]; then
    return 0
  fi

  current_branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
  current_branch="$(gh_afk_strip_cr "$current_branch")"
  if [ "$current_branch" = "$expected_branch" ]; then
    return 0
  fi

  status="$(git status --short 2>/dev/null || true)"
  if [ -n "$status" ]; then
    echo "Refusing to hand AFK execution over to Treehouse from a dirty checkout." >&2
    echo "Move, commit, or discard these changes before switching AFK into its dedicated worktree:" >&2
    echo "$status" >&2
    return 1
  fi

  treehouse="$(gh_afk_resolve_treehouse)"

  worktree_path="$(gh_afk_treehouse_worktree_for_holder "$treehouse" "$holder")"
  if [ -z "$worktree_path" ]; then
    worktree_path="$(gh_afk_acquire_treehouse_worktree "$treehouse" "$holder")"
  fi

  if [ ! -d "$worktree_path" ]; then
    echo "Treehouse worktree path does not exist: $worktree_path" >&2
    return 1
  fi

  gh_afk_prepare_treehouse_branch "$worktree_path" "$expected_branch" "$base_branch"

  echo "Using Treehouse worktree: $worktree_path"
  echo "Using AFK branch: $expected_branch"

  GH_AFK_TREEHOUSE_ACTIVE="$holder" AFK_EXPECTED_BRANCH="$expected_branch" \
    exec "$worktree_path/$script_rel" "$@"
}

gh_afk_parent_issue_number() {
  local body="$1"
  local body_file
  local parent
  local refs

  body_file="$(mktemp)"
  printf '%s\n' "$body" > "$body_file"
  parent="$(gh_afk_extract_section "Parent" "$body_file")"
  rm -f "$body_file"

  if ! printf '%s' "$parent" | gh_afk_has_text; then
    return 0
  fi

  refs="$(gh_afk_issue_refs_from_text "$parent")"
  if [ -n "$refs" ]; then
    printf '%s\n' "$refs" | head -n 1
  fi
}

gh_afk_check_issue_candidate() {
  local number="$1"
  local title="$2"
  local body="$3"
  local body_file
  local contract_body
  local what_to_build
  local acceptance
  local blocked_by
  local parent
  local refs
  local dep
  local dep_state

  if contract_body="$(gh_afk_resolve_issue_contract_body "$number" "$body")"; then
    body="$contract_body"
  fi

  body_file="$(mktemp)"
  printf '%s\n' "$body" > "$body_file"

  what_to_build="$(gh_afk_extract_section "What to build" "$body_file")"
  if ! printf '%s' "$what_to_build" | gh_afk_has_text; then
    echo "Malformed ready-for-agent issue #$number ($title): missing ## What to build in the issue body and structured comments." >&2
    rm -f "$body_file"
    return 2
  fi

  acceptance="$(gh_afk_extract_section "Acceptance criteria" "$body_file")"
  if ! printf '%s' "$acceptance" | gh_afk_has_text; then
    echo "Malformed ready-for-agent issue #$number ($title): missing ## Acceptance criteria in the issue body and structured comments." >&2
    rm -f "$body_file"
    return 2
  fi

  parent="$(gh_afk_extract_section "Parent" "$body_file")"
  if printf '%s' "$parent" | gh_afk_has_text \
    && ! printf '%s' "$parent" | grep -Eiq '^[[:space:]]*None\b'; then
    refs="$(gh_afk_issue_refs_from_text "$parent")"
    if [ -z "$refs" ]; then
      echo "Malformed ready-for-agent issue #$number ($title): ## Parent must reference a GitHub issue." >&2
      rm -f "$body_file"
      return 2
    fi
  fi

  blocked_by="$(gh_afk_extract_section "Blocked by" "$body_file")"
  if ! printf '%s' "$blocked_by" | gh_afk_has_text; then
    echo "Malformed ready-for-agent issue #$number ($title): missing ## Blocked by in the issue body and structured comments." >&2
    rm -f "$body_file"
    return 2
  fi

  rm -f "$body_file"

  if printf '%s\n' "$blocked_by" | grep -Eiq '^[[:space:]]*None([[:space:]]*\.[[:space:]]*|[[:space:]]*-[[:space:]]*can start immediately[[:space:]]*)$'; then
    return 0
  fi

  refs="$(gh_afk_issue_refs_from_text "$blocked_by")"
  if [ -z "$refs" ]; then
    echo "Malformed ready-for-agent issue #$number ($title): ## Blocked by must be None or GitHub issue references." >&2
    return 2
  fi

  for dep in $refs; do
    if ! dep_state="$(gh_afk_retry_read gh issue view "$dep" --json state --jq '.state')"; then
      echo "Cannot evaluate ready-for-agent issue #$number ($title): GitHub is unavailable while reading blocker #$dep." >&2
      return 3
    fi
    if [ "$dep_state" != "CLOSED" ]; then
      echo "Skipping issue #$number: blocker #$dep is $dep_state." >&2
      return 1
    fi
  done

  return 0
}

gh_afk_issue_last_claim_agent() {
  local number="$1"
  local agent_name="$2"
  local claim_body

  if ! claim_body="$(gh_afk_retry_read gh issue view "$number" --json comments --jq '
    (.comments // [])
    | map(.body // "")
    | map(select(test("^Claimed by .+ AFK loop at ")))
    | last // ""
  ')"; then
    echo "Cannot inspect claim history for issue #$number." >&2
    return 2
  fi
  claim_body="$(gh_afk_strip_cr "$claim_body")"

  case "$claim_body" in
    "Claimed by $agent_name AFK loop at "*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

gh_afk_select_resumable_issue() {
  local agent_name="$1"
  local issues_file="$2"
  local numbers
  local number
  local title
  local body
  local check_status
  local claim_status
  local skipped_for_dependency=0

  mapfile -t numbers < <(
    jq -r '
      sort_by(.number)[]
      | ([.labels[].name] // []) as $names
      | select((($names | index("prd")) == null)
          and (($names | index("blocked")) == null)
          and (($names | index("in-progress")) != null))
      | .number
    ' "$issues_file"
  )

  for number in "${numbers[@]}"; do
    number="$(gh_afk_strip_cr "$number")"
    if [ -z "$number" ]; then
      continue
    fi

    set +e
    gh_afk_issue_last_claim_agent "$number" "$agent_name"
    claim_status="$?"
    set -e

    case "$claim_status" in
      0)
        ;;
      1)
        continue
        ;;
      *)
        return "$claim_status"
        ;;
    esac

    title="$(jq -r --argjson number "$number" '.[] | select(.number == $number) | .title' "$issues_file")"
    body="$(jq -r --argjson number "$number" '.[] | select(.number == $number) | .body // ""' "$issues_file")"

    set +e
    gh_afk_check_issue_candidate "$number" "$title" "$body"
    check_status="$?"
    set -e

    case "$check_status" in
      0)
        printf 'resume\t%s\n' "$number"
        return 0
        ;;
      1)
        skipped_for_dependency=1
        ;;
      *)
        return "$check_status"
        ;;
    esac
  done

  if [ "$skipped_for_dependency" -eq 1 ]; then
    return 12
  fi

  return 11
}

gh_afk_select_issue() {
  local agent_name="$1"
  local issues_file
  local selection
  local resume_status
  local ready_count
  local numbers
  local number
  local title
  local body
  local check_status
  local skipped_for_dependency=0

  issues_file="$(mktemp)"
  if ! gh_afk_retry_read gh issue list \
    --state open \
    --label ready-for-agent \
    --limit 1000 \
    --json number,title,url,labels,body > "$issues_file"; then
    rm -f "$issues_file"
    return 2
  fi

  ready_count="$(jq 'length' "$issues_file")"
  ready_count="$(gh_afk_strip_cr "$ready_count")"
  if [ "$ready_count" -eq 0 ]; then
    rm -f "$issues_file"
    return 10
  fi

  set +e
  selection="$(gh_afk_select_resumable_issue "$agent_name" "$issues_file")"
  resume_status="$?"
  set -e

  case "$resume_status" in
    0)
      printf '%s\n' "$selection"
      rm -f "$issues_file"
      return 0
      ;;
    11)
      ;;
    12)
      skipped_for_dependency=1
      ;;
    *)
      rm -f "$issues_file"
      return "$resume_status"
      ;;
  esac

  mapfile -t numbers < <(
    jq -r '
      sort_by(.number)[]
      | ([.labels[].name] // []) as $names
      | select((($names | index("prd")) == null)
          and (($names | index("blocked")) == null)
          and (($names | index("in-progress")) == null))
      | .number
    ' "$issues_file"
  )

  if [ "${#numbers[@]}" -eq 0 ]; then
    rm -f "$issues_file"
    return 11
  fi

  for number in "${numbers[@]}"; do
    number="$(gh_afk_strip_cr "$number")"
    if [ -z "$number" ]; then
      continue
    fi

    title="$(jq -r --argjson number "$number" '.[] | select(.number == $number) | .title' "$issues_file")"
    body="$(jq -r --argjson number "$number" '.[] | select(.number == $number) | .body // ""' "$issues_file")"

    set +e
    gh_afk_check_issue_candidate "$number" "$title" "$body"
    check_status="$?"
    set -e

    case "$check_status" in
      0)
        printf 'new\t%s\n' "$number"
        rm -f "$issues_file"
        return 0
        ;;
      1)
        skipped_for_dependency=1
        ;;
      *)
        rm -f "$issues_file"
        return "$check_status"
        ;;
    esac
  done

  rm -f "$issues_file"
  if [ "$skipped_for_dependency" -eq 1 ]; then
    return 12
  fi
  return 11
}

gh_afk_render_issue_context() {
  local number="$1"
  local body
  local contract_body
  local parent_number

  body="$(gh_afk_retry_read gh issue view "$number" --json body --jq '.body // ""')"
  if contract_body="$(gh_afk_resolve_issue_contract_body "$number" "$body")"; then
    body="$contract_body"
  fi
  parent_number="$(gh_afk_parent_issue_number "$body")"

  echo "# Selected GitHub issue"
  echo ""
  gh_afk_retry_read gh issue view "$number" --comments
  echo ""

  if printf '%s' "$body" | gh_afk_has_text; then
    echo "# Resolved issue contract"
    echo ""
    printf '%s\n' "$body"
    echo ""
  fi

  if [ -n "$parent_number" ]; then
    echo "# Parent issue context"
    echo ""
    gh_afk_retry_read gh issue view "$parent_number" --comments
    echo ""
  fi
}

gh_afk_rollback_claim_label() {
  local number="$1"
  local attempts="$2"
  local delay="$3"
  local attempt
  local error_file

  error_file="$(mktemp)"
  for ((attempt=1; attempt<=attempts; attempt++)); do
    if gh issue edit "$number" --remove-label "in-progress" >/dev/null 2>"$error_file"; then
      rm -f "$error_file"
      return 0
    fi
    if gh_afk_retry_read gh issue view "$number" --json labels --jq '([.labels[].name] // []) | index("in-progress") == null' 2>/dev/null | grep -Fxq true; then
      rm -f "$error_file"
      return 0
    fi
    if [ "$attempt" -ge "$attempts" ] || ! gh_afk_is_transient_github_error "$error_file"; then
      break
    fi
    [ "$delay" -eq 0 ] || sleep "$delay"
  done
  cat "$error_file" >&2
  rm -f "$error_file"
  return 1
}

gh_afk_claim_issue() {
  local number="$1"
  local agent_name="$2"
  local timestamp
  local body
  local attempts="${GH_AFK_RETRY_ATTEMPTS:-5}"
  local delay="${GH_AFK_RETRY_BASE_DELAY_SECONDS:-2}"
  local attempt
  local error_file
  local status

  timestamp="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  body="Claimed by $agent_name AFK loop at $timestamp."
  if ! [[ "$attempts" =~ ^[1-9][0-9]*$ ]]; then
    attempts=5
  fi
  if ! [[ "$delay" =~ ^[0-9]+$ ]]; then
    delay=2
  fi

  error_file="$(mktemp)"
  for ((attempt=1; attempt<=attempts; attempt++)); do
    if gh issue edit "$number" --add-label "in-progress" >/dev/null 2>"$error_file"; then
      break
    else
      status="$?"
    fi
    if gh_afk_retry_read gh issue view "$number" --json labels --jq '([.labels[].name] // []) | index("in-progress") != null' 2>/dev/null | grep -Fxq true; then
      break
    fi
    if [ "$attempt" -ge "$attempts" ] || ! gh_afk_is_transient_github_error "$error_file"; then
      cat "$error_file" >&2
      rm -f "$error_file"
      if ! gh_afk_rollback_claim_label "$number" "$attempts" "$delay"; then
        echo "Failed to roll back incomplete claim for issue #$number." >&2
      fi
      return "$status"
    fi
    [ "$delay" -eq 0 ] || sleep "$delay"
  done

  for ((attempt=1; attempt<=attempts; attempt++)); do
    if gh_afk_retry_read gh issue view "$number" --json comments 2>/dev/null | jq -e --arg body "$body" 'any(.comments[]?; .body == $body)' >/dev/null; then
      rm -f "$error_file"
      return 0
    fi
    : >"$error_file"
    if gh issue comment "$number" --body "$body" >/dev/null 2>"$error_file"; then
      rm -f "$error_file"
      return 0
    else
      status="$?"
    fi
    if gh_afk_retry_read gh issue view "$number" --json comments 2>/dev/null | jq -e --arg body "$body" 'any(.comments[]?; .body == $body)' >/dev/null; then
      rm -f "$error_file"
      return 0
    fi
    if [ "$attempt" -ge "$attempts" ] || ! gh_afk_is_transient_github_error "$error_file"; then
      break
    fi
    [ "$delay" -eq 0 ] || sleep "$delay"
  done

  cat "$error_file" >&2
  rm -f "$error_file"
  if ! gh_afk_rollback_claim_label "$number" "$attempts" "$delay"; then
    echo "Failed to roll back incomplete claim for issue #$number." >&2
  fi
  return "$status"
}

gh_afk_release_issue_claim() {
  local number="$1"

  gh issue edit "$number" --remove-label "in-progress" >/dev/null
}
