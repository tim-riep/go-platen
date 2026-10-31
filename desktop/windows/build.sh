#!/usr/bin/env bash
# Builds Platen.exe (windows/amd64) and the installer Platen-<version>-setup.exe
# in desktop/windows/build. Runs on macOS and Linux; needs Go and makensis.
#
#   ./build.sh             sign Platen.exe, the installer and the uninstaller
#   ./build.sh --no-sign   unsigned (local testing, CI)
#
# Signing uses the Certum certificate through ssign: CERTUM_EMAIL and
# CERTUM_OTP must be set (see sign.sh). VERSION overrides the version from
# `git describe` (v1.2.3 -> 1.2.3).
set -euo pipefail

cd "$(dirname "$0")"
SIGN=1
for arg in "$@"; do
	case "$arg" in
	--no-sign) SIGN=0 ;;
	*) echo "unknown option $arg" >&2; exit 2 ;;
	esac
done

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)}
# Windows version resources need four numbers; other builds get 0.0.0.0.
NUM=$(echo "$VERSION" | sed -n 's/^\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p')
NUM=${NUM:-0.0.0}
BUILD=build
rm -rf "$BUILD"
mkdir -p "$BUILD"

echo "==> Building Platen.exe $VERSION"
go run github.com/tc-hib/go-winres@v0.3.3 make --in winres.json --out ../../cmd/platen-desktop/rsrc \
	--arch amd64 --file-version "$NUM.0" --product-version "$NUM.0"
(cd ../.. && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
	-ldflags "-s -w -H=windowsgui -X main.version=$VERSION" -o "desktop/windows/$BUILD/Platen.exe" ./cmd/platen-desktop)
rm -f ../../cmd/platen-desktop/rsrc_windows_*.syso

flags=()
if [ $SIGN = 1 ]; then
	./sign.sh "$BUILD/Platen.exe"
	flags=(-DSIGN "-DSIGNER=$PWD/sign.sh")
fi

echo "==> Building the installer"
# makensis aborts without a UTF-8 locale.
if [ "$(uname -s)" = Darwin ]; then export LC_ALL=en_US.UTF-8; else export LC_ALL=C.UTF-8; fi
makensis -V2 -DVERSION="$VERSION" -DVERSION4="$NUM.0" -DBUILD="$BUILD" ${flags[@]+"${flags[@]}"} platen.nsi
echo "==> $BUILD/Platen-$VERSION-setup.exe"
