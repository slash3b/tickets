# syntax=docker/dockerfile:1.2
FROM golang:1.25-alpine AS builder

WORKDIR /go/src/app

# Copy go.mod and go.sum first for caching dependencies
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg \
    go mod tidy

# Copy the rest of your source code (including cmd/app)
COPY . .

# Build your app, specifying the main package location
RUN --mount=type=cache,target=/go/pkg \
    CGO_ENABLED=0 go build -v -o /go/bin/cineplex ./cmd/app

# Prepare final minimal image
FROM alpine:latest

WORKDIR /app

RUN apk add --no-cache tzdata

COPY --from=builder /go/bin/cineplex /app/

ENTRYPOINT ["/app/cineplex"]