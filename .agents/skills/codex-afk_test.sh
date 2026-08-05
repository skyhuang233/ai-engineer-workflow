#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

test_afk_entrypoint_runs_the_fenced_control_plane() (
  local sandbox
  local bin_dir
  local call_log
  local workflow_stub
  local output
  local status

  sandbox="$(mktemp -d)"
  trap 'rm -rf "$sandbox"' EXIT
  bin_dir="$sandbox/bin"
  call_log="$sandbox/calls.log"
  workflow_stub="$sandbox/workflow-stub"
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

  cat > "$workflow_stub" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$AFK_TEST_CALL_LOG"
case "$1" in
  gateway)
    shift
    ready_file=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --ready-file) ready_file="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    test -n "$ready_file"
    printf '%s\n' 'http://127.0.0.1:43123' > "$ready_file"
    trap 'printf "%s\n" gateway-stopped >> "$AFK_TEST_CALL_LOG"; exit 0' TERM INT
    while :; do sleep 1; done
    ;;
  poll-github)
    exit 0
    ;;
  *)
    echo "unexpected workflow command: $*" >&2
    exit 91
    ;;
esac
EOF

  cat > "$bin_dir/go" <<EOF
#!/usr/bin/env bash
set -euo pipefail
test "\$1" = build
shift
output=""
while [ "\$#" -gt 0 ]; do
  case "\$1" in
    -o) output="\$2"; shift 2 ;;
    *) shift ;;
  esac
done
test -n "\$output"
cp '$workflow_stub' "\$output"
chmod +x "\$output"
EOF

  cat > "$bin_dir/codex" <<'EOF'
#!/usr/bin/env bash
echo 'legacy host Codex execution must not run' >&2
exit 92
EOF

  cat > "$bin_dir/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\n' "$*" >> "$AFK_TEST_CALL_LOG"
EOF

  chmod +x "$bin_dir/git" "$bin_dir/gh" "$bin_dir/go" "$bin_dir/codex" "$bin_dir/curl" "$workflow_stub"

  export PATH="$bin_dir:$PATH"
  export AFK_TEST_CALL_LOG="$call_log"
  export WORKFLOW_DATABASE="$sandbox/workflow.db"
  export WORKFLOW_RUNTIME_ROOT="$sandbox/runtime"
  export WORKFLOW_MAX_PARALLEL_RUNS=3
  export WORKFLOW_GATEWAY_START_TIMEOUT_SECONDS=5

  set +e
  output="$($SCRIPT_DIR/codex-afk.sh 2 2>&1)"
  status="$?"
  set -e
  if [ "$status" -ne 0 ]; then
    printf '%s\n' "$output" >&2
    echo "expected the AFK control-plane entrypoint to succeed, got $status" >&2
    return 1
  fi

  if [ "$(grep -c '^poll-github ' "$call_log")" -ne 2 ]; then
    printf '%s\n' "$output" >&2
    cat "$call_log" >&2
    echo "expected exactly two bounded poll-github passes" >&2
    return 1
  fi
  if ! grep -Eq '^gateway .*--listen 127\.0\.0\.1:0 .*--ready-file ' "$call_log"; then
    cat "$call_log" >&2
    echo "expected an ephemeral credential-isolated Gateway" >&2
    return 1
  fi
  if ! grep -Eq '^poll-github .*--once .*--repository owner/repo .*--root 2 ' "$call_log"; then
    cat "$call_log" >&2
    echo "expected poll-github to receive the discovered repository and plan root" >&2
    return 1
  fi
  if ! grep -Eq '^poll-github .*--gateway-url http://127\.0\.0\.1:43123 ' "$call_log"; then
    cat "$call_log" >&2
    echo "expected poll-github to use the ready Gateway URL" >&2
    return 1
  fi
  if ! grep -Eq '^poll-github .*--max-parallel-runs 3 ' "$call_log"; then
    cat "$call_log" >&2
    echo "expected the configured global parallelism limit" >&2
    return 1
  fi
  if ! grep -Fxq 'gateway-stopped' "$call_log"; then
    cat "$call_log" >&2
    echo "expected the bounded AFK entrypoint to stop its Gateway" >&2
    return 1
  fi
  if grep -Fq 'WORKFLOW_GITHUB_GATEWAY_COMMAND' <<<"$output"; then
    printf '%s\n' "$output" >&2
    echo "legacy Gateway-command bootstrap must not be required" >&2
    return 1
  fi
)

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
echo "codex-afk tests passed"
