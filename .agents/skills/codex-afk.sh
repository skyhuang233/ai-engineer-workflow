#!/usr/bin/env bash
set -euo pipefail

ITERATIONS=100

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
PROMPT_FILE="$PROJECT_ROOT/.agents/skills/ralph/prompt.md"
TRACKER_FILE="$PROJECT_ROOT/docs/agents/issue-tracker.md"
COMMON_FILE="$PROJECT_ROOT/.agents/skills/github-afk-common.sh"

if [ ! -f "$PROMPT_FILE" ]; then
  echo "Prompt file not found: $PROMPT_FILE"
  exit 1
fi

if [ ! -f "$COMMON_FILE" ]; then
  echo "Common helper not found: $COMMON_FILE"
  exit 1
fi

# shellcheck source=/dev/null
source "$COMMON_FILE"

cd "$PROJECT_ROOT"
gh_afk_require_tools
gh_afk_ensure_labels

for ((i=1; i<=ITERATIONS; i++)); do
  echo ""
  echo "===== Codex AFK iteration $i / $ITERATIONS ====="
  echo ""

  set +e
  selection="$(gh_afk_select_issue "Codex")"
  select_status="$?"
  set -e

  if [ "$select_status" -eq 10 ]; then
    echo "No open ready-for-agent GitHub issues found."
    echo "<promise>NO MORE TASKS</promise>"
    echo "Codex AFK loop complete before iteration $i."
    exit 0
  fi

  if [ "$select_status" -eq 11 ]; then
    echo "No unclaimed executable GitHub issues found."
    echo "Codex AFK loop complete before iteration $i."
    exit 0
  fi

  if [ "$select_status" -eq 12 ]; then
    echo "No unblocked ready-for-agent GitHub issues found."
    echo "Codex AFK loop complete before iteration $i."
    exit 0
  fi

  if [ "$select_status" -ne 0 ]; then
    echo "Failed to select a GitHub issue for AFK work."
    exit "$select_status"
  fi

  selection_mode="${selection%%$'\t'*}"
  issue_number="${selection#*$'\t'}"
  claimed_issue=0
  if [ -z "$selection_mode" ] || [ -z "$issue_number" ] || [ "$selection_mode" = "$selection" ]; then
    echo "Malformed issue selection result: $selection" >&2
    exit 1
  fi

  if [ "$selection_mode" = "resume" ]; then
    echo "Continuing with the current worktree state for GitHub issue #$issue_number."
    echo "Resuming GitHub issue #$issue_number."
  else
    gh_afk_assert_clean_worktree
    gh_afk_claim_issue "$issue_number" "Codex"
    claimed_issue=1
    echo "Claimed GitHub issue #$issue_number."
  fi

  prompt_file="$(mktemp)"
  result_file="$(mktemp)"

  if ! {
    commits="$(
      git log -n 5 --format="%H%n%ad%n%B---" --date=short 2>/dev/null \
        || echo "No commits found"
    )"
    current_branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "UNKNOWN")"

    {
      echo "# Previous commits"
      echo ""
      echo "$commits"
      echo ""

      echo "# Issue tracker convention"
      echo ""
      cat "$TRACKER_FILE"
      echo ""

      echo "# Current git status"
      echo ""
      git status --short
      echo ""

      echo "# Execution checkout"
      echo ""
      echo "Project root: $PROJECT_ROOT"
      echo "Current branch: $current_branch"
      echo ""

      gh_afk_render_issue_context "$issue_number"

      echo "# Ralph prompt"
      echo ""
      cat "$PROMPT_FILE"

      echo ""
      echo "# GitHub issue workflow override"
      echo ""
      echo "This command is an explicit user instruction to continue the autonomous AFK loop."
      echo "Do not ask the user for confirmation."
      echo "Work only on GitHub issue #$issue_number."
      echo "The issue has already been claimed with the in-progress label."
      echo "Do not use local issues/ files as the task source."
      echo "Append progress and completion notes to GitHub issue #$issue_number comments."
      echo "If complete, remove in-progress and close GitHub issue #$issue_number."
      echo "If blocked, remove in-progress and add blocked or needs-info."
      echo "Stay in the current checkout rooted at $PROJECT_ROOT."
      echo "Do not switch branches, use Treehouse or sibling checkouts, or edit files outside $PROJECT_ROOT."
    } > "$prompt_file"
  }; then
    rm -f "$prompt_file" "$result_file"
    if [ "$claimed_issue" -eq 1 ]; then
      if gh_afk_release_issue_claim "$issue_number"; then
        echo "Released GitHub issue #$issue_number after bootstrap failure." >&2
      else
        echo "Failed to release GitHub issue #$issue_number after bootstrap failure." >&2
      fi
    fi
    echo "Failed to prepare AFK prompt context." >&2
    exit 1
  fi

  codex exec \
    --cd "$PROJECT_ROOT" \
    --model gpt-5.6-luna \
    -c 'model_reasoning_effort="high"' \
    --dangerously-bypass-approvals-and-sandbox \
    --output-last-message "$result_file" \
    - < "$prompt_file"

  result="$(cat "$result_file")"
  rm -f "$prompt_file" "$result_file"

  if [[ "$result" == *"<promise>NO MORE TASKS</promise>"* ]]; then
    echo "Codex AFK loop complete after $i iterations."
    exit 0
  fi
done
