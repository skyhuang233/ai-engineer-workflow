FROM node:22-bookworm-slim@sha256:f32b81066cde10a75dbac96646099533316d94bac4150c55da1636e1f0ffdc46

ARG CODEX_VERSION=0.148.0
ARG GITHUB_CLI_VERSION=2.97.0
ARG GITHUB_CLI_LINUX_AMD64_SHA256=18c4aef8dc35414ad15e4eda3a7e7a3a61df03e4219a92d8eed8064fe439ce94
ARG GITHUB_CLI_COMMIT=55dbb4dc6b7edb10b48e3d7fc5bccd32318d1b55
ARG GITHUB_CLI_X_MOD_VERSION=v0.40.0
ARG GITHUB_CLI_BUILD_DATE=2026-07-31
ARG GO_VERSION=1.26.6
ARG GO_LINUX_AMD64_SHA256=708effb774be8237570d0add163225abbdfaf4fca28b2611df167beba4feef89
ARG NO_MISTAKES_VERSION=v1.41.2
ARG NO_MISTAKES_REPOSITORY=skyhuang233/no-mistakes
ARG NO_MISTAKES_COMMIT=eafc10e0fc7306be3af1750524aa2067e5048942

ENV DEBIAN_SNAPSHOT=20260713T000000Z \
    APT_PACKAGES="ca-certificates=20230311+deb12u1 curl=7.88.1-10+deb12u15 git=1:2.39.5-0+deb12u3 jq=1.6-2.1+deb12u2 procps=2:4.0.2-3 sqlite3=3.40.1-2+deb12u2" \
    PATH="/usr/local/go/bin:${PATH}"

LABEL org.opencontainers.image.source="https://github.com/skyhuang233/ai-engineer-workflow"
LABEL io.workflow.codex.version="${CODEX_VERSION}"
LABEL io.workflow.github-cli.version="${GITHUB_CLI_VERSION}"
LABEL io.workflow.github-cli.linux-amd64-sha256="${GITHUB_CLI_LINUX_AMD64_SHA256}"
LABEL io.workflow.github-cli.commit="${GITHUB_CLI_COMMIT}"
LABEL io.workflow.github-cli.x-mod-version="${GITHUB_CLI_X_MOD_VERSION}"
LABEL io.workflow.go.version="${GO_VERSION}"
LABEL io.workflow.go.linux-amd64-sha256="${GO_LINUX_AMD64_SHA256}"
LABEL io.workflow.no-mistakes.version="${NO_MISTAKES_VERSION}"
LABEL io.workflow.no-mistakes.repository="${NO_MISTAKES_REPOSITORY}"
LABEL io.workflow.no-mistakes.commit="${NO_MISTAKES_COMMIT}"
LABEL io.workflow.debian.snapshot="${DEBIAN_SNAPSHOT}"
LABEL io.workflow.apt.packages="${APT_PACKAGES}"

RUN rm -f /etc/apt/sources.list /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT} bookworm main" > /etc/apt/sources.list \
    && apt-get update -o Acquire::Check-Valid-Until=false -o Acquire::https::Verify-Peer=false \
    && apt-get install --yes --no-install-recommends -o Acquire::https::Verify-Peer=false ca-certificates=20230311+deb12u1 \
    && apt-get update -o Acquire::Check-Valid-Until=false \
    && apt-get install --yes --no-install-recommends ${APT_PACKAGES} \
    && rm -rf /var/lib/apt/lists/*

RUN npm install --global "@openai/codex@${CODEX_VERSION}" \
    && rm --recursive --force /usr/local/lib/node_modules/npm /usr/local/bin/npm /usr/local/bin/npx \
    && test ! -e /usr/local/lib/node_modules/npm \
    && codex --version

RUN curl --fail --show-error --location --retry 5 --retry-all-errors --retry-delay 2 \
      "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
      --output /tmp/go.tar.gz \
    && echo "${GO_LINUX_AMD64_SHA256}  /tmp/go.tar.gz" | sha256sum --check --strict \
    && tar --extract --gzip --file /tmp/go.tar.gz --directory /usr/local \
    && rm /tmp/go.tar.gz \
    && go version

RUN retry() { \
      attempt=1; \
      until "$@"; do \
        if [ "$attempt" = 5 ]; then return 1; fi; \
        sleep "$((attempt * 2))"; \
        attempt="$((attempt + 1))"; \
      done; \
    }; \
    for attempt in 1 2 3 4 5; do \
      rm --recursive --force /tmp/gh; \
      if git clone --filter=blob:none --no-checkout https://github.com/cli/cli.git /tmp/gh; then break; fi; \
      if [ "$attempt" = 5 ]; then exit 1; fi; \
      sleep "$((attempt * 2))"; \
    done \
    && git -C /tmp/gh checkout --detach "${GITHUB_CLI_COMMIT}" \
    && test "$(git -C /tmp/gh rev-parse HEAD)" = "${GITHUB_CLI_COMMIT}" \
    && test -z "$(git -C /tmp/gh status --porcelain)" \
    && cd /tmp/gh \
    && go mod edit -require="golang.org/x/mod@${GITHUB_CLI_X_MOD_VERSION}" \
    && retry go mod download "golang.org/x/mod@${GITHUB_CLI_X_MOD_VERSION}" \
    && retry env CGO_ENABLED=0 GOFLAGS=-mod=mod go build -buildvcs=false -trimpath \
      -ldflags "-buildid= -s -w -X github.com/cli/cli/v2/internal/build.Version=${GITHUB_CLI_VERSION} -X github.com/cli/cli/v2/internal/build.Date=${GITHUB_CLI_BUILD_DATE}" \
      -o /usr/local/bin/gh ./cmd/gh \
    && echo "${GITHUB_CLI_LINUX_AMD64_SHA256}  /usr/local/bin/gh" | sha256sum --check --strict \
    && go version -m /usr/local/bin/gh | grep --extended-regexp "^[[:space:]]*dep[[:space:]]+golang\\.org/x/mod[[:space:]]+${GITHUB_CLI_X_MOD_VERSION}([[:space:]]|$)" \
    && rm --recursive --force /tmp/gh /root/go /root/.cache/go-build \
    && gh version

COPY go.mod go.sum /tmp/workflow/
COPY internal/deliverysource /tmp/workflow/internal/deliverysource
COPY cmd/delivery-source-digest /tmp/workflow/cmd/delivery-source-digest
RUN cd /tmp/workflow \
    && go build -trimpath -o /usr/local/bin/delivery-source-digest ./cmd/delivery-source-digest \
    && rm --recursive --force /tmp/workflow \
    && delivery-source-digest 2>&1 | grep --fixed-strings "usage: delivery-source-digest"

RUN for attempt in 1 2 3 4 5; do \
      rm --recursive --force /tmp/no-mistakes; \
      if git clone --filter=blob:none --no-checkout \
        "https://github.com/${NO_MISTAKES_REPOSITORY}.git" /tmp/no-mistakes; then break; fi; \
      if [ "$attempt" = 5 ]; then exit 1; fi; \
      sleep "$((attempt * 2))"; \
    done \
    && git -C /tmp/no-mistakes checkout --detach "${NO_MISTAKES_COMMIT}" \
    && test "$(git -C /tmp/no-mistakes rev-parse HEAD)" = "${NO_MISTAKES_COMMIT}" \
    && test -z "$(git -C /tmp/no-mistakes status --porcelain)" \
    && no_mistakes_short_commit="$(git -C /tmp/no-mistakes rev-parse --short=7 HEAD)" \
    && no_mistakes_commit_date="$(git -C /tmp/no-mistakes show -s --format=%cI HEAD)" \
    && cd /tmp/no-mistakes \
    && CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -buildvcs=true -trimpath \
      -ldflags "-buildid= -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Version=${NO_MISTAKES_VERSION} -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Commit=${no_mistakes_short_commit} -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Date=${no_mistakes_commit_date}" \
      -o /usr/local/bin/no-mistakes ./cmd/no-mistakes \
    && test -z "$(git status --porcelain)" \
    && rm --recursive --force /tmp/no-mistakes /root/go /root/.cache/go-build \
    && no-mistakes --version

WORKDIR /workspace
