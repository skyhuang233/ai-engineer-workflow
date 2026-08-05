#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

test_afk_entrypoint_runs_the_fenced_control_plane() (
  local sandbox
  local bin_dir
  local call_log
  local output
  local status

  sandbox="$(mktemp -d)"
  trap 'rm -rf "$sandbox"' EXIT
  bin_dir="$sandbox/bin"
  call_log="$sandbox/calls.log"
  mkdir -p "$bin_dir"

  cat > "$bin_dir/git" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  "fetch origin") exit 0 ;;
  "status --short") exit 0 ;;
  "rev-parse HEAD") printf '%s\n' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' ;;
  "rev-parse refs/remotes/origin/main") printf '%s\n' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' ;;
  *) echo "unexpected git call: $*" >&2; exit 89 ;;
esac
EOF

  cat > "$bin_dir/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  "auth status") exit 0 ;;
  "repo view") printf '%s\n' 'owner/repo' ;;
  "issue list") printf '%s\n' '2' ;;
  *) echo "unexpected gh call: $*" >&2; exit 90 ;;
esac
EOF

  cat > "$bin_dir/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
test "$1 $2" = "run ./cmd/workflow"
shift 2
printf '%s\n' "$*" >> "$AFK_TEST_CALL_LOG"
test "$1" = afk
EOF

  cat > "$bin_dir/codex" <<'EOF'
#!/usr/bin/env bash
echo 'legacy host Codex execution must not run' >&2
exit 92
EOF

  chmod +x "$bin_dir/git" "$bin_dir/gh" "$bin_dir/go" "$bin_dir/codex"

  export PATH="$bin_dir:$PATH"
  export AFK_TEST_CALL_LOG="$call_log"
  export WORKFLOW_DATABASE="$sandbox/workflow.db"
  export WORKFLOW_RUNTIME_ROOT="$sandbox/runtime"
  export WORKFLOW_MAX_PARALLEL_RUNS=3
  export WORKFLOW_POLL_INTERVAL=7s

  set +e
  output="$($SCRIPT_DIR/codex-afk.sh 2 2>&1)"
  status="$?"
  set -e
  if [ "$status" -ne 0 ]; then
    printf '%s\n' "$output" >&2
    echo "expected the AFK control-plane entrypoint to succeed, got $status" >&2
    return 1
  fi

  if [ "$(grep -c '^afk ' "$call_log")" -ne 1 ]; then
    printf '%s\n' "$output" >&2
    cat "$call_log" >&2
    echo "expected exactly one in-process Control Plane invocation" >&2
    return 1
  fi
  if ! grep -Eq '^afk .*--iterations 2 .*--repository owner/repo .*--root 2 ' "$call_log"; then
    cat "$call_log" >&2
    echo "expected AFK to receive the bounded iterations, repository, and plan root" >&2
    return 1
  fi
  if ! grep -Eq '^afk .*--max-parallel-runs 3 ' "$call_log"; then
    cat "$call_log" >&2
    echo "expected the configured global parallelism limit" >&2
    return 1
  fi
  if ! grep -Eq '^afk .*--poll-interval 7s ' "$call_log"; then
    cat "$call_log" >&2
    echo "expected the configured persistent polling cadence" >&2
    return 1
  fi
  if grep -Fq 'WORKFLOW_GITHUB_GATEWAY_COMMAND' <<<"$output"; then
    printf '%s\n' "$output" >&2
    echo "legacy Gateway-command bootstrap must not be required" >&2
    return 1
  fi
)

test_powershell_entrypoint_forwards_the_bounded_run() {
  local entrypoint="$SCRIPT_DIR/codex-afk.ps1"
  if [ ! -f "$entrypoint" ]; then
    echo "PowerShell AFK entrypoint is missing" >&2
    return 1
  fi
  if ! grep -Fq '[Parameter(Mandatory = $true, Position = 0)]' "$entrypoint" ||
     ! grep -Fq './codex-afk.sh $Iterations' "$entrypoint"; then
    echo "PowerShell AFK entrypoint no longer validates and forwards Iterations" >&2
    return 1
  fi
}

test_afk_entrypoint_refuses_unaccepted_source() (
  local sandbox
  local bin_dir
  local output
  local status

  sandbox="$(mktemp -d)"
  trap 'rm -rf "$sandbox"' EXIT
  bin_dir="$sandbox/bin"
  mkdir -p "$bin_dir"

  cat > "$bin_dir/git" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  "fetch origin") exit 0 ;;
  "status --short") exit 0 ;;
  "rev-parse HEAD") printf '%s\n' 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' ;;
  "rev-parse refs/remotes/origin/main") printf '%s\n' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' ;;
  *) exit 89 ;;
esac
EOF
  cat > "$bin_dir/gh" <<'EOF'
#!/usr/bin/env bash
if [ "$1 $2" = "auth status" ]; then exit 0; fi
echo "GitHub must not be read from an unaccepted source revision" >&2
exit 90
EOF
  cat > "$bin_dir/go" <<EOF
#!/usr/bin/env bash
printf 'built\n' > '$sandbox/unexpected-build'
exit 91
EOF
  chmod +x "$bin_dir/git" "$bin_dir/gh" "$bin_dir/go"

  export PATH="$bin_dir:$PATH"
  set +e
  output="$($SCRIPT_DIR/codex-afk.sh 1 2>&1)"
  status="$?"
  set -e
  if [ "$status" -eq 0 ]; then
    echo "expected an unaccepted source revision to be rejected" >&2
    return 1
  fi
  if ! grep -Fq 'accepted origin/main' <<<"$output"; then
    printf '%s\n' "$output" >&2
    echo "expected a clear accepted-main preflight error" >&2
    return 1
  fi
  if [ -e "$sandbox/unexpected-build" ]; then
    echo "unaccepted source reached the Control Plane build" >&2
    return 1
  fi
)

test_afk_entrypoint_runs_the_fenced_control_plane
test_afk_entrypoint_refuses_unaccepted_source
test_powershell_entrypoint_forwards_the_bounded_run
echo "codex-afk tests passed"
