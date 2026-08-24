# Build from the REPO ROOT, not from this directory:
#   docker build -f services/hello/Dockerfile .
# The build needs pkg/ and go.mod, which live above this file.
FROM golang:1.25-alpine AS build
WORKDIR /src

# Copy manifests first so dependency download is cached independently of source
# changes — editing main.go should not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY pkg/ pkg/
COPY services/hello/ services/hello/

# CGO_ENABLED=0 gives a static binary, which is what lets the final stage be
# scratch. -s -w strip the symbol table and DWARF: smaller image, and nothing here
# is ever debugged by attaching gdb to a container.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/hello ./services/hello

FROM scratch
# The CA bundle is needed for any outbound TLS. scratch has nothing at all.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/hello /hello
# Non-root by uid: scratch has no /etc/passwd, so a username would not resolve.
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/hello"]
