FROM node:20-bookworm-slim

ARG CODEX_VERSION=0.144.4

RUN apt-get update \
    && apt-get install --yes --no-install-recommends ca-certificates git \
    && npm install --global "@openai/codex@${CODEX_VERSION}" \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY gatemole-codex-entrypoint.sh /usr/local/bin/gatemole-codex

ENTRYPOINT ["/usr/local/bin/gatemole-codex"]
