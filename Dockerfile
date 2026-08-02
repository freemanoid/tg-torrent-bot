# syntax=docker/dockerfile:1

# Multi-stage build producing a small static image for linux/arm64 (Umbrel
# Raspberry Pi 4B) as well as the host platform.
#
#   docker build --platform linux/arm64 -t tg-torrent-bot:latest .
#
# The build stage runs on the build host's native platform and cross-compiles
# (pure Go, CGO disabled — modernc.org/sqlite needs no C toolchain), so the
# build is fast even when targeting ARM64 from an x86 machine.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

# Final image: alpine for CA certificates (Telegram/Prowlarr HTTPS) and a
# shell for occasional debugging; still ~10 MB.
FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /out/bot /usr/local/bin/bot

# SQLite database lives here; mount a volume (see docker-compose.yml).
VOLUME /data

ENTRYPOINT ["/usr/local/bin/bot"]
