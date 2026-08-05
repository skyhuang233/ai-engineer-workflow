#!/usr/bin/env bash
set -euo pipefail

ITERATIONS="${1:-100}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMMON_FILE="$PROJECT_ROOT/.agents/skills/github-afk-common.sh"

if ! [[ "$ITERATIONS" =~ ^[1-9][0-9]*$ ]]; then
  echo "Iterations must be a positive integer." >&2
  exit 2
fi
if [ ! -f "$COMMON_FILE" ]; then
  echo "Common helper not found: $COMMON_FILE" >&2
  exit 1
fi

# shellcheck source=/dev/null
source "$COMMON_FILE"

cd "$PROJECT_ROOT"
gh_afk_require_tools
for required in go; do
  if ! command -v "$required" >/dev/null 2>&1; then
    echo "Required command not found on PATH: $required" >&2
    exit 1
  fi
done

git fetch origin main
if [ -n "$(git status --short)" ]; then
  echo "AFK requires a clean accepted source checkout." >&2
  exit 1
fi
source_head="$(gh_afk_strip_cr "$(git rev-parse HEAD)")"
accepted_head="$(gh_afk_strip_cr "$(git rev-parse refs/remotes/origin/main)")"
if [ "$source_head" != "$accepted_head" ]; then
  echo "AFK must run from the accepted origin/main revision." >&2
  echo "Current HEAD: $source_head" >&2
  echo "Accepted main: $accepted_head" >&2
  exit 1
fi

database="${WORKFLOW_DATABASE:-}"
if [ -n "$database" ]; then
  database="$(gh_afk_to_bash_path "$database")"
else
  if [ -n "${PROGRAMDATA:-}" ]; then
    database="$(gh_afk_to_bash_path "$PROGRAMDATA")/workflow/workflow.db"
  else
    database="$PROJECT_ROOT/.workflow/workflow.db"
  fi
fi
runtime_root="${WORKFLOW_RUNTIME_ROOT:-}"
if [ -n "$runtime_root" ]; then
  runtime_root="$(gh_afk_to_bash_path "$runtime_root")"
else
  runtime_root="$(dirname "$database")"
fi
workspace_root="${WORKFLOW_WORKSPACE_ROOT:-}"
if [ -n "$workspace_root" ]; then
  workspace_root="$(gh_afk_to_bash_path "$workspace_root")"
else
  workspace_root="$runtime_root/workspaces"
fi
state_root="${WORKFLOW_CODEX_STATE_ROOT:-}"
if [ -n "$state_root" ]; then
  state_root="$(gh_afk_to_bash_path "$state_root")"
else
  state_root="$runtime_root/codex-state"
fi
config_path="${WORKFLOW_CONFIG:-$PROJECT_ROOT/config/toolchain.json}"
config_path="$(gh_afk_to_bash_path "$config_path")"
max_parallel_runs="${WORKFLOW_MAX_PARALLEL_RUNS:-1}"
workspace_retention="${WORKFLOW_WORKSPACE_RETENTION:-168h}"
poll_interval="${WORKFLOW_POLL_INTERVAL:-1m}"

if ! [[ "$max_parallel_runs" =~ ^[1-9][0-9]*$ ]]; then
  echo "WORKFLOW_MAX_PARALLEL_RUNS must be a positive integer." >&2
  exit 2
fi
if [ ! -f "$config_path" ]; then
  echo "Toolchain config not found: $config_path" >&2
  exit 1
fi

repository="$(gh_afk_retry_read gh repo view --json nameWithOwner --jq '.nameWithOwner')"
repository="$(gh_afk_strip_cr "$repository")"
if [ -z "$repository" ]; then
  echo "Cannot resolve the current GitHub repository." >&2
  exit 1
fi

mapfile -t plan_roots < <(
  gh_afk_retry_read gh issue list \
    --state open \
    --label workflow:plan \
    --limit 100 \
    --json number \
    --jq '.[].number'
)
if [ "${#plan_roots[@]}" -ne 1 ]; then
  echo "AFK requires exactly one open workflow:plan issue; found ${#plan_roots[@]}." >&2
  exit 1
fi
plan_root="$(gh_afk_strip_cr "${plan_roots[0]}")"
if ! [[ "$plan_root" =~ ^[1-9][0-9]*$ ]]; then
  echo "Invalid workflow:plan issue number: $plan_root" >&2
  exit 1
fi

mkdir -p "$(dirname "$database")" "$workspace_root" "$state_root"
unset WORKFLOW_GITHUB_GATEWAY_COMMAND || true

echo "Using production Control Plane for $repository plan #$plan_root."
echo "Maximum parallel Worker Runs: $max_parallel_runs"

go run ./cmd/workflow afk \
  --iterations "$ITERATIONS" \
  --repository "$repository" \
  --root "$plan_root" \
  --source "$PROJECT_ROOT" \
  --workspace-root "$workspace_root" \
  --state-root "$state_root" \
  --max-parallel-runs "$max_parallel_runs" \
  --workspace-retention "$workspace_retention" \
  --poll-interval "$poll_interval" \
  --config "$config_path" \
  --database "$database"
