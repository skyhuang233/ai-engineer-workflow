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
for required in go curl od tr; do
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
if [ -z "$database" ]; then
  if [ -n "${PROGRAMDATA:-}" ]; then
    database="$(gh_afk_to_bash_path "$PROGRAMDATA")/workflow/workflow.db"
  else
    database="$PROJECT_ROOT/.workflow/workflow.db"
  fi
fi
runtime_root="${WORKFLOW_RUNTIME_ROOT:-$(dirname "$database")}"
workspace_root="${WORKFLOW_WORKSPACE_ROOT:-$runtime_root/workspaces}"
state_root="${WORKFLOW_CODEX_STATE_ROOT:-$runtime_root/codex-state}"
config_path="${WORKFLOW_CONFIG:-$PROJECT_ROOT/config/toolchain.json}"
max_parallel_runs="${WORKFLOW_MAX_PARALLEL_RUNS:-1}"
workspace_retention="${WORKFLOW_WORKSPACE_RETENTION:-168h}"
gateway_start_timeout="${WORKFLOW_GATEWAY_START_TIMEOUT_SECONDS:-30}"

if ! [[ "$max_parallel_runs" =~ ^[1-9][0-9]*$ ]]; then
  echo "WORKFLOW_MAX_PARALLEL_RUNS must be a positive integer." >&2
  exit 2
fi
if ! [[ "$gateway_start_timeout" =~ ^[1-9][0-9]*$ ]]; then
  echo "WORKFLOW_GATEWAY_START_TIMEOUT_SECONDS must be a positive integer." >&2
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
runtime_dir="$(mktemp -d)"
ready_file="$runtime_dir/gateway.ready"
gateway_stdout="$runtime_dir/gateway.stdout.log"
gateway_stderr="$runtime_dir/gateway.stderr.log"
workflow_binary="$runtime_dir/workflow"
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) workflow_binary="$runtime_dir/workflow.exe" ;;
esac
gateway_pid=""

cleanup() {
  local status="$?"
  trap - EXIT
  if [ -n "$gateway_pid" ]; then
    if kill -0 "$gateway_pid" 2>/dev/null; then
      kill "$gateway_pid" 2>/dev/null || true
    fi
    wait "$gateway_pid" 2>/dev/null || true
  fi
  rm -rf "$runtime_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

go build -o "$workflow_binary" ./cmd/workflow
control_token="$(od -An -N32 -tx1 /dev/urandom | tr -d '[:space:]')"
if [ "${#control_token}" -ne 64 ]; then
  echo "Failed to generate the ephemeral Gateway control token." >&2
  exit 1
fi
export WORKFLOW_GATEWAY_CONTROL_TOKEN="$control_token"
unset WORKFLOW_GITHUB_GATEWAY_COMMAND || true

"$workflow_binary" gateway \
  --config "$config_path" \
  --database "$database" \
  --listen 127.0.0.1:0 \
  --ready-file "$ready_file" \
  >"$gateway_stdout" 2>"$gateway_stderr" &
gateway_pid="$!"

ready_attempts=$((gateway_start_timeout * 10))
for ((attempt=1; attempt<=ready_attempts; attempt++)); do
  if [ -s "$ready_file" ]; then
    break
  fi
  if ! kill -0 "$gateway_pid" 2>/dev/null; then
    cat "$gateway_stdout" >&2 || true
    cat "$gateway_stderr" >&2 || true
    echo "GitHub Write Gateway exited before becoming ready." >&2
    exit 1
  fi
  sleep 0.1
done
if [ ! -s "$ready_file" ]; then
  cat "$gateway_stdout" >&2 || true
  cat "$gateway_stderr" >&2 || true
  echo "GitHub Write Gateway did not become ready within ${gateway_start_timeout}s." >&2
  exit 1
fi
gateway_url="$(gh_afk_strip_cr "$(cat "$ready_file")")"
if ! curl --fail --silent --show-error \
  --header "X-Workflow-Control-Token: $control_token" \
  --output /dev/null \
  "$gateway_url/healthz"; then
  echo "GitHub Write Gateway readiness probe failed." >&2
  exit 1
fi

echo "Using production Control Plane for $repository plan #$plan_root."
echo "Gateway: $gateway_url"
echo "Maximum parallel Worker Runs: $max_parallel_runs"

for ((iteration=1; iteration<=ITERATIONS; iteration++)); do
  echo ""
  echo "===== Codex AFK control-plane pass $iteration / $ITERATIONS ====="
  echo ""
  "$workflow_binary" poll-github \
    --once \
    --repository "$repository" \
    --root "$plan_root" \
    --source "$PROJECT_ROOT" \
    --workspace-root "$workspace_root" \
    --state-root "$state_root" \
    --gateway-url "$gateway_url" \
    --max-parallel-runs "$max_parallel_runs" \
    --workspace-retention "$workspace_retention" \
    --config "$config_path" \
    --database "$database"
done
