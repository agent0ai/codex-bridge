FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go config.example.yaml ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /codex-bridge .

FROM node:22-bookworm-slim AS codex
ARG CODEX_VERSION=0.164.0
RUN npm install -g @openai/codex@${CODEX_VERSION} && \
    find /usr/local/lib/node_modules/@openai/codex -type f -path '*/vendor/*/bin/codex' -exec cp '{}' /codex-native ';' && \
    test -x /codex-native

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git micro python3 python3-yaml ripgrep && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /codex-bridge /usr/local/bin/codex-bridge
COPY --from=codex /codex-native /usr/local/bin/codex
# Codex subscription models can use the JavaScript execution tool.
COPY --from=codex /usr/local/bin/node /usr/local/bin/node
COPY tests/smoke.py /opt/bridge-smoke.py
COPY tests/attachments.py /opt/bridge-attachments.py
COPY tests/cache.py /opt/bridge-cache.py
WORKDIR /work
CMD ["sleep", "infinity"]
