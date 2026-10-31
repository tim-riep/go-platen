#!/bin/sh
# Builds the .deb packages of the Linux desktop client for amd64 and arm64
# in desktop/linux/build. Runs on Linux and macOS with Go; nfpm (MIT) is
# run with `go run`.
#
# Environment: VERSION overrides the version from `git describe`
# (v1.2.3 -> 1.2.3; builds without a tag get 0.0.0~git<commit>).
set -eu

cd "$(dirname "$0")"
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)}
# Builds after a tag sort above it (1.2.3+4.gabc), prereleases below the
# release (1.2.3-rc1 -> 1.2.3~rc1).
case "$VERSION" in
[0-9]*) DEBVERSION=$(echo "$VERSION" | sed -e 's/-\([0-9]*\)-g\([0-9a-f]*\)/+\1.g\2/' -e 's/-dirty$/.dirty/' | tr - '~') ;;
*) DEBVERSION="0.0.0~git$(echo "$VERSION" | tr -c 'A-Za-z0-9.\n' '.')" ;;
esac
rm -rf build
for arch in amd64 arm64; do
	echo "==> platen-desktop $DEBVERSION $arch"
	mkdir -p build/stage
	# nfpm takes the binary from a fixed path.
	(cd ../.. && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$VERSION" -o desktop/linux/build/stage/platen-desktop ./cmd/platen-desktop)
	GOARCH=$arch VERSION=$DEBVERSION go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0 \
		package --config nfpm.yaml --packager deb --target build/
done
rm -rf build/stage
ls build/*.deb
