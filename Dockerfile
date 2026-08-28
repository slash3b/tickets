# syntax=docker/dockerfile:1.9
#
# ONE Dockerfile for every service, selected by a build arg:
#
#   docker build --build-arg SERVICE=bank -t tickets-bank .
#
# Eight near-identical Dockerfiles would drift — someone fixes a CVE in one, bumps
# Go in another, forgets the rest. The services differ in their code, not in how
# they are packaged, so packaging lives in one place. Every service therefore
# keeps its main package at services/<name>/cmd/.

# BUILDPLATFORM, not TARGETPLATFORM: the compiler runs natively and cross-compiles,
# which is far faster than emulating the target under QEMU. Go makes this free.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS build

WORKDIR /src

# Manifests first, so editing source does not invalidate the dependency layer.
# A single-line passwd for the nonroot uid, so the scratch image has a real user
# entry rather than a bare number.
RUN echo 'nobody:x:65534:65534:nobody:/:/sbin/nologin' > /etc/passwd.nobody

COPY go.mod go.sum ./
# Cache mounts, not layers: the module cache survives across builds AND across
# source changes, instead of being re-downloaded whenever go.sum moves.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY pkg/ pkg/
COPY services/ services/
# Generated protobuf and gRPC stubs. Copied explicitly, like everything else here:
# the .dockerignore allowlist controls what CAN be sent, these COPY lines control
# what IS. Both had to learn about gen/ — fixing only the first is why this failed
# twice.
COPY gen/ gen/

ARG SERVICE
ARG VERSION=dev
# Supplied by BuildKit. Defaulted so a plain `docker build` still works.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Fail loudly on a missing SERVICE rather than building something arbitrary.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    test -n "${SERVICE}" || { echo 'SERVICE build-arg is required' >&2; exit 1; }; \
    test -d "services/${SERVICE}/cmd" || { echo "services/${SERVICE}/cmd does not exist" >&2; exit 1; }; \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/app "./services/${SERVICE}/cmd"
#   -trimpath    strips absolute build paths, so the binary does not embed
#                /home/runner/... and two builds of the same source match.
#   -buildvcs=false  .git is excluded by .dockerignore; without this Go errors
#                rather than silently omitting the stamp.
#   -s -w        drop the symbol table and DWARF. Nothing here is debugged by
#                attaching a debugger to a running container.

# scratch has literally nothing, so everything the binary needs must be explicit.
# That is the point: there is no shell, no package manager and no libc for an
# attacker to reach, and nothing unaudited can appear in the image later.
FROM scratch

# Outbound TLS needs a CA bundle. Copied from the build stage rather than
# installed, so the final image gains no package manager.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# scratch has no /etc/passwd, so `USER 65534` is a bare uid with no name behind
# it. Most things cope, but anything calling user.Current() fails. One line fixes
# it, and keeps the image free of a real passwd file.
COPY --from=build /etc/passwd.nobody /etc/passwd

COPY --from=build /out/app /app

USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/app"]

# OCI metadata. The source label is what makes ghcr link the package back to this
# repository and show its README — without it the package page is an orphan.
ARG SERVICE
ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/slash3b/tickets" \
      org.opencontainers.image.title="tickets-${SERVICE}" \
      org.opencontainers.image.revision="${VERSION}" \
      org.opencontainers.image.licenses="MIT"
