# syntax=docker/dockerfile:1.7

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ENV CGO_ENABLED=0 GOOS=linux
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    for cmd in bot worker remindctl; do \
      go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/$cmd ./cmd/$cmd; \
    done; \
    mkdir -p /data-init; \
    touch /data-init/.keep

# debian-slim + official Google Chrome: required for headless price scraping.
# Debian-packaged Chromium has a different BoringSSL/h2 fingerprint that
# Qrator WAF detects as a bot. Official Chrome ships the same BoringSSL as
# the real browser, so its TLS/h2 fingerprint passes WAF inspection.
# Set price.headless: false in config.yaml to skip Chrome entirely.
FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
    && curl -fsSL https://dl.google.com/linux/linux_signing_key.pub \
        | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg \
    && echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] \
        http://dl.google.com/linux/chrome/deb/ stable main" \
        > /etc/apt/sources.list.d/google-chrome.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends google-chrome-stable \
    && apt-get purge -y curl gnupg \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 nonroot \
    && useradd  --system --uid 65532 --gid nonroot --no-create-home nonroot
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY --from=build --chown=nonroot:nonroot /data-init/ /data/
COPY config.yaml.example /app/config.yaml
ENV DATABASE_DRIVER=sqlite DATABASE_DSN=/data/remind.db
USER nonroot:nonroot
