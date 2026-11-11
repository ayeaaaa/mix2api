# syntax=docker/dockerfile:1.7
#
# Reproducible multi-arch runtime image. Build with:
#   docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/ayeaaaa/mix2api:v2.0.5 .
#
# CGO is disabled and the dashboard is embedded in the binary, so the runtime
# stage needs no toolchain, shell scripts, or writable application directory.

FROM golang:1.22-alpine AS build
ARG VERSION=2.0.5
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
# Declare without defaults so buildx's automatic platform args are inherited.
# Defaults are applied in the RUN fallback for non-buildx builds.
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -buildvcs=false -buildmode=exe \
    -ldflags "-s -w -buildid= \
      -X github.com/ayeaaaa/mix2api-v2/internal/core/version.Version=${VERSION} \
      -X github.com/ayeaaaa/mix2api-v2/internal/core/version.Commit=${COMMIT} \
      -X github.com/ayeaaaa/mix2api-v2/internal/core/version.Date=${BUILD_DATE}" \
    -o /out/mix2api ./cmd/mix2api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 10001 mix2api \
 && adduser -S -D -H -u 10001 -G mix2api mix2api \
 && mkdir -p /app/accounts /app/usage /app/media \
 && chown -R mix2api:mix2api /app/accounts /app/usage /app/media \
 && chmod 0555 /app
COPY --from=build /out/mix2api /app/mix2api
COPY LICENSE /app/LICENSE
WORKDIR /app
USER 10001:10001
ENV HOST=0.0.0.0 \
    PORT=58675 \
    ACCOUNTS_DIR=/app/accounts \
    USAGE_DIR=/app/usage \
    WB_MEDIA_DIR=/app/media \
    TZ=UTC
VOLUME ["/app/accounts", "/app/usage", "/app/media"]
EXPOSE 58675
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${PORT}/health" || exit 1
ENTRYPOINT ["/app/mix2api"]
