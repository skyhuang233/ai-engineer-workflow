FROM node:22-bookworm-slim@sha256:f32b81066cde10a75dbac96646099533316d94bac4150c55da1636e1f0ffdc46

ARG CODEX_VERSION=0.146.0
ARG NO_MISTAKES_VERSION=v1.41.2
ARG NO_MISTAKES_FORK_RELEASE=workflow-v1.41.2.0
ARG NO_MISTAKES_SHA256=a100c58bdfe7df9f598ecec32553d5fbd8eb0079912fc830f362011fd9dc8825

LABEL org.opencontainers.image.source="https://github.com/skyhuang233/workflow"
LABEL io.workflow.codex.version="${CODEX_VERSION}"
LABEL io.workflow.no-mistakes.version="${NO_MISTAKES_VERSION}"

RUN apt-get update \
    && apt-get install --yes --no-install-recommends ca-certificates curl gh git jq sqlite3 \
    && rm -rf /var/lib/apt/lists/*

RUN npm install --global "@openai/codex@${CODEX_VERSION}" \
    && codex --version

RUN curl --fail --show-error --location \
      "https://github.com/skyhuang233/no-mistakes/releases/download/${NO_MISTAKES_FORK_RELEASE}/no-mistakes-${NO_MISTAKES_VERSION}-linux-amd64.tar.gz" \
      --output /tmp/no-mistakes.tar.gz \
    && echo "${NO_MISTAKES_SHA256}  /tmp/no-mistakes.tar.gz" | sha256sum --check --strict \
    && tar --extract --gzip --file /tmp/no-mistakes.tar.gz --directory /usr/local/bin \
    && rm /tmp/no-mistakes.tar.gz \
    && no-mistakes --version

WORKDIR /workspace
