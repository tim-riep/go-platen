# syntax=docker/dockerfile:1

# platen container image: one static, cgo-free binary running as a non-root
# user. NetBird runs in userspace mode, so the container needs no
# capabilities and publishes no ports.

FROM --platform=$BUILDPLATFORM golang:1.26.4-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/platen ./cmd/platen \
 && mkdir -p /out/state/platen /out/state/platen-worker /out/tmp /out/etc \
 && printf 'NAME="platen"\nID=platen\nVERSION_ID="%s"\nPRETTY_NAME="platen %s"\n' "$VERSION" "$VERSION" > /out/etc/os-release

# The runtime image is built from scratch and contains only the platen
# binary with its license notices, the CA certificate bundle (needed for TLS
# to the NetBird management server) and empty directories. Containers bind
# /etc/resolv.conf and /etc/hosts themselves.
FROM scratch
ARG VERSION=dev
LABEL org.opencontainers.image.title="platen" \
      org.opencontainers.image.description="Print management server with distributed print workers" \
      org.opencontainers.image.source="https://github.com/tim-riep/go-platen" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
# Mozilla's CA certificates as packaged by Debian (MPL-2.0), unmodified.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/share/doc/ca-certificates/copyright /usr/share/doc/ca-certificates/copyright
# NetBird reports the operating system to its management server.
COPY --from=build /out/etc/os-release /etc/os-release
COPY --from=build /out/platen /usr/local/bin/platen
COPY LICENSE THIRD_PARTY_LICENSES /usr/share/doc/platen/
COPY --from=build --chown=65532:65532 /out/tmp /tmp
# State directories for the server (/var/lib/platen) and the worker
# (/var/lib/platen-worker), owned by the non-root user so that named volumes
# mounted there are writable.
COPY --from=build --chown=65532:65532 /out/state/ /var/lib/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/platen"]
CMD ["server", "-config", "/etc/platen/server.toml"]
