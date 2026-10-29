# syntax=docker/dockerfile:1.7
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/bob-factory ./cmd/bob-factory

# BOB compiles its linux-elf minions at build time, so the image keeps the Go
# toolchain. It runs as an unprivileged user with its own build cache, warmed
# with the standard library for both platforms so a first build is quick.
FROM golang:1.25-alpine
RUN adduser -D -u 65532 bob
USER bob
ENV GOCACHE=/home/bob/.cache/go-build \
    GOTOOLCHAIN=local \
    CGO_ENABLED=0
RUN GOOS=linux GOARCH=amd64 go build std && GOOS=linux GOARCH=386 go build std
COPY --from=builder /out/bob-factory /app/bob-factory
ENTRYPOINT ["/app/bob-factory"]
