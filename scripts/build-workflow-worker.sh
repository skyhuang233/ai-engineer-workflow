#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
  echo "usage: build-workflow-worker.sh <image-reference>" >&2
  exit 2
fi

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
toolchain="$repository_root/config/toolchain.json"
image_reference="$1"

pin() {
  jq --exit-status --raw-output "$1" "$toolchain"
}

docker build \
  --build-arg "CODEX_VERSION=$(pin '.codex.version')" \
  --build-arg "GITHUB_CLI_VERSION=$(pin '.github_cli.version')" \
  --build-arg "GITHUB_CLI_LINUX_AMD64_SHA256=$(pin '.github_cli.linux_amd64_sha256')" \
  --build-arg "GO_VERSION=$(pin '.go.version')" \
  --build-arg "GO_LINUX_AMD64_SHA256=$(pin '.go.linux_amd64_sha256')" \
  --build-arg "NO_MISTAKES_VERSION=$(pin '.no_mistakes.version')" \
  --build-arg "NO_MISTAKES_REPOSITORY=$(pin '.no_mistakes.repository')" \
  --build-arg "NO_MISTAKES_COMMIT=$(pin '.no_mistakes.commit')" \
  --tag "$image_reference" \
  --file "$repository_root/deploy/worker/Dockerfile" \
  "$repository_root"
