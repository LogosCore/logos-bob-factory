# syntax=docker/dockerfile:1.7
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/bob-factory ./cmd/bob-factory

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/bob-factory /app/bob-factory
ENTRYPOINT ["/app/bob-factory"]
