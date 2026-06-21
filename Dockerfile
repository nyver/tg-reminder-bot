# syntax=docker/dockerfile:1.7

FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ENV CGO_ENABLED=0 GOOS=linux
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    for cmd in bot worker api remindctl; do \
      go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/$cmd ./cmd/$cmd; \
    done; \
    mkdir -p /data-init; \
    touch /data-init/.keep

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY --from=build --chown=nonroot:nonroot /data-init/ /data/
COPY config.yaml.example /app/config.yaml
ENV DATABASE_DRIVER=sqlite DATABASE_DSN=/data/remind.db
USER nonroot:nonroot
