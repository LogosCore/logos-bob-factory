# syntax=docker/dockerfile:1.7
FROM golang:1.25-alpine AS builder
WORKDIR /src
# Our own modules come straight from GitHub (git), not through the public Go
# proxy: a tag builds the moment it is pushed, whatever the proxy has cached.
# go.sum still checks them.
ENV GOPRIVATE=github.com/logoscore/*
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/bob-factory ./cmd/bob-factory

# BOB compiles its minions (linux-elf, windows-exe) at build time, so the
# image keeps the Go toolchain and the minion's sources (/app/minion, a
# module of its own). It runs as an unprivileged user with its own build
# cache, warmed with the standard library for linux/amd64, linux/386 and
# windows/amd64 (some 180 MB each), with the -trimpath BOB builds with, so a
# build for those compiles only the minion; windows/386 and windows/arm64
# compile the standard library on their first build, kept for the next. Its module cache is warmed with the
# minion's modules too: a build needs no network until the minion needs a
# module the image lacks, which comes through GOPROXY (set it to a mirror,
# Nexus say, where there is no internet; go.sum checks it either way). The
# warm-up leaves out replace lines: a local checkout of a module during
# development is mounted at run time, not baked in. It takes our own modules
# straight from GitHub (GOPRIVATE, git), as the build stage does; a minion's
# build keeps GOPROXY, whatever it points at.
FROM golang:1.25-alpine
RUN apk add --no-cache git && adduser -D -u 65532 bob
USER bob
ENV GOCACHE=/home/bob/.cache/go-build \
    GOMODCACHE=/home/bob/go/pkg/mod \
    GOTOOLCHAIN=local \
    CGO_ENABLED=0
RUN GOOS=linux GOARCH=amd64 go build -trimpath std && GOOS=linux GOARCH=386 go build -trimpath std && \
    GOOS=windows GOARCH=amd64 go build -trimpath std
COPY --from=builder /out/bob-factory /app/bob-factory
COPY minion/ /app/minion/
RUN cp -r /app/minion /tmp/warm && cd /tmp/warm && sed -i '/^replace /d' go.mod && \
    GOPRIVATE=github.com/logoscore/* go mod download && cd / && rm -rf /tmp/warm
ENTRYPOINT ["/app/bob-factory"]
