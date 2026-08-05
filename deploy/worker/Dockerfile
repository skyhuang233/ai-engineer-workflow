FROM node:22-bookworm-slim@sha256:f32b81066cde10a75dbac96646099533316d94bac4150c55da1636e1f0ffdc46

ARG CODEX_VERSION=0.146.0
ARG NO_MISTAKES_VERSION=v1.41.2
ARG NO_MISTAKES_UPSTREAM_COMMIT=867d64d9c2df89f3f204ad1f5528e5bf7b460caa
ARG NO_MISTAKES_FORK_REPOSITORY=skyhuang233/no-mistakes
ARG NO_MISTAKES_FORK_RELEASE=workflow-v1.41.2.0
ARG NO_MISTAKES_SHA256=a100c58bdfe7df9f598ecec32553d5fbd8eb0079912fc830f362011fd9dc8825

ENV DEBIAN_SNAPSHOT=20260713T000000Z \
    APT_PACKAGES="ca-certificates=20230311+deb12u1 curl=7.88.1-10+deb12u15 gh=2.23.0+dfsg1-1 git=1:2.39.5-0+deb12u3 jq=1.6-2.1+deb12u2 sqlite3=3.40.1-2+deb12u2"

LABEL org.opencontainers.image.source="https://github.com/skyhuang233/workflow"
LABEL io.workflow.codex.version="${CODEX_VERSION}"
LABEL io.workflow.no-mistakes.version="${NO_MISTAKES_VERSION}"
LABEL io.workflow.no-mistakes.upstream-commit="${NO_MISTAKES_UPSTREAM_COMMIT}"
LABEL io.workflow.debian.snapshot="${DEBIAN_SNAPSHOT}"
LABEL io.workflow.apt.packages="${APT_PACKAGES}"

RUN rm -f /etc/apt/sources.list /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT} bookworm main" > /etc/apt/sources.list \
    && apt-get update -o Acquire::Check-Valid-Until=false \
    && apt-get install --yes --no-install-recommends ${APT_PACKAGES} \
    && rm -rf /var/lib/apt/lists/*

RUN npm install --global "@openai/codex@${CODEX_VERSION}" \
    && codex --version

RUN curl --fail --show-error --location \
      "https://github.com/${NO_MISTAKES_FORK_REPOSITORY}/releases/download/${NO_MISTAKES_FORK_RELEASE}/no-mistakes-${NO_MISTAKES_VERSION}-linux-amd64.tar.gz" \
      --output /tmp/no-mistakes.tar.gz \
    && echo "${NO_MISTAKES_SHA256}  /tmp/no-mistakes.tar.gz" | sha256sum --check --strict \
    && tar --extract --gzip --file /tmp/no-mistakes.tar.gz --directory /usr/local/bin \
    && rm /tmp/no-mistakes.tar.gz \
    && no-mistakes --version

WORKDIR /workspace
