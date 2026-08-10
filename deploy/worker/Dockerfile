FROM node:22-bookworm-slim@sha256:f32b81066cde10a75dbac96646099533316d94bac4150c55da1636e1f0ffdc46

ARG CODEX_VERSION=0.147.0
ARG GITHUB_CLI_VERSION=2.97.0
ARG GITHUB_CLI_LINUX_AMD64_SHA256=a2c9b8497e1f85b1ad0dfcb78b5a622e098801b8e461e459e88e1ee12f018112
ARG GO_VERSION=1.25.12
ARG GO_LINUX_AMD64_SHA256=234828b7a89e0e303d2556310ee549fbcf253d28de937bac3da13d6294262ac1
ARG NO_MISTAKES_VERSION=v1.41.2
ARG NO_MISTAKES_UPSTREAM_COMMIT=867d64d9c2df89f3f204ad1f5528e5bf7b460caa
ARG NO_MISTAKES_FORK_REPOSITORY=skyhuang233/no-mistakes
ARG NO_MISTAKES_FORK_COMMIT=e073fd0dc51c64004468b04de8cf2ab50cd5d177
ARG NO_MISTAKES_FORK_RELEASE=workflow-v1.41.2.2
ARG NO_MISTAKES_SHA256=bd19d18ba20af1905b0f13ac6b0f9c283d299da48279da7dd0a5ff2242a58d4c

ENV DEBIAN_SNAPSHOT=20260713T000000Z \
    APT_PACKAGES="ca-certificates=20230311+deb12u1 curl=7.88.1-10+deb12u15 git=1:2.39.5-0+deb12u3 jq=1.6-2.1+deb12u2 procps=2:4.0.2-3 sqlite3=3.40.1-2+deb12u2" \
    PATH="/usr/local/go/bin:${PATH}"

LABEL org.opencontainers.image.source="https://github.com/skyhuang233/ai-engineer-workflow"
LABEL io.workflow.codex.version="${CODEX_VERSION}"
LABEL io.workflow.github-cli.version="${GITHUB_CLI_VERSION}"
LABEL io.workflow.github-cli.linux-amd64-sha256="${GITHUB_CLI_LINUX_AMD64_SHA256}"
LABEL io.workflow.go.version="${GO_VERSION}"
LABEL io.workflow.go.linux-amd64-sha256="${GO_LINUX_AMD64_SHA256}"
LABEL io.workflow.no-mistakes.version="${NO_MISTAKES_VERSION}"
LABEL io.workflow.no-mistakes.upstream-commit="${NO_MISTAKES_UPSTREAM_COMMIT}"
LABEL io.workflow.no-mistakes.fork-commit="${NO_MISTAKES_FORK_COMMIT}"
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

RUN curl --fail --show-error --location \
      "https://github.com/cli/cli/releases/download/v${GITHUB_CLI_VERSION}/gh_${GITHUB_CLI_VERSION}_linux_amd64.tar.gz" \
      --output /tmp/gh.tar.gz \
    && echo "${GITHUB_CLI_LINUX_AMD64_SHA256}  /tmp/gh.tar.gz" | sha256sum --check --strict \
    && tar --extract --gzip --file /tmp/gh.tar.gz --directory /tmp \
    && install --mode 0755 "/tmp/gh_${GITHUB_CLI_VERSION}_linux_amd64/bin/gh" /usr/local/bin/gh \
    && rm --recursive --force /tmp/gh.tar.gz "/tmp/gh_${GITHUB_CLI_VERSION}_linux_amd64" \
    && gh version

RUN curl --fail --show-error --location \
      "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
      --output /tmp/go.tar.gz \
    && echo "${GO_LINUX_AMD64_SHA256}  /tmp/go.tar.gz" | sha256sum --check --strict \
    && tar --extract --gzip --file /tmp/go.tar.gz --directory /usr/local \
    && rm /tmp/go.tar.gz \
    && go version

RUN curl --fail --show-error --location \
      "https://github.com/${NO_MISTAKES_FORK_REPOSITORY}/releases/download/${NO_MISTAKES_FORK_RELEASE}/no-mistakes-${NO_MISTAKES_VERSION}-linux-amd64.tar.gz" \
      --output /tmp/no-mistakes.tar.gz \
    && echo "${NO_MISTAKES_SHA256}  /tmp/no-mistakes.tar.gz" | sha256sum --check --strict \
    && tar --extract --gzip --file /tmp/no-mistakes.tar.gz --directory /usr/local/bin \
    && rm /tmp/no-mistakes.tar.gz \
    && no-mistakes --version

WORKDIR /workspace
