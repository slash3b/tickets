# ONE Dockerfile for every service. Which one it builds is a build arg.
#
# Eight near-identical Dockerfiles would drift: someone would fix a CVE in one,
# bump Go in another, and forget the rest. The services differ in their code, not
# in how they are packaged, so the packaging lives in one place.
#
#   docker build --build-arg SERVICE=bank -t tickets-bank .
#
# Every service must therefore keep its main package at services/<name>/cmd/.
FROM golang:1.25-alpine AS build
WORKDIR /src

# Manifests first, so editing source does not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY pkg/ pkg/
COPY services/ services/

ARG SERVICE
ARG VERSION=dev
# CGO_ENABLED=0 gives a static binary, which is what lets the final stage be
# scratch. -s -w strip symbols and DWARF: nothing here is ever debugged by
# attaching a debugger to a container.
RUN test -n "$SERVICE" || (echo "SERVICE build-arg is required" >&2; exit 1) && \
    CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/app ./services/${SERVICE}/cmd

FROM scratch
# scratch has nothing at all, and any outbound TLS needs the CA bundle.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/app /app
# Non-root by uid: scratch has no /etc/passwd, so a username would not resolve.
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/app"]
