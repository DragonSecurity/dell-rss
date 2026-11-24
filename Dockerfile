# syntax=docker/dockerfile:1@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183

# The builder runs on the build host's architecture and cross-compiles, so
# multi-arch images build at native speed without QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/dell-rss . \
 && mkdir -p /out/data

# distroless/static: CA certificates and tzdata, no shell, runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=builder /out/dell-rss /usr/local/bin/dell-rss
COPY --from=builder --chown=65532:65532 /out/data /data

ENV PORT=8088 \
    CACHE_DIR=/data

VOLUME /data
EXPOSE 8088

HEALTHCHECK --interval=30s --timeout=5s --start-period=5m --retries=3 \
  CMD ["/usr/local/bin/dell-rss", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/dell-rss"]
