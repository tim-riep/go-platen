#!/bin/sh
# Builds Platen.app (universal) and the installer package Platen.pkg in
# desktop/macos/build. Signs with Developer ID and notarizes by default.
#
#   ./build.sh                 sign, package, notarize, staple
#   ./build.sh --no-notarize   sign and package only
#   ./build.sh --no-sign       unsigned app and package (local testing)
#
# Environment:
#   VERSION                version, default from `git describe` (v1.2.3 -> 1.2.3)
#   PLATEN_BUNDLE_ID       bundle identifier, default io.github.tim-riep.platen
#   PLATEN_SIGN_APP        codesign identity, default "Developer ID Application"
#   PLATEN_SIGN_PKG        productbuild identity, default "Developer ID Installer"
#   Notarization, one of:
#   NOTARY_KEY_PATH, NOTARY_KEY_ID, NOTARY_ISSUER_ID
#                          App Store Connect API key (.p8), used in CI
#   PLATEN_NOTARY_PROFILE  notarytool keychain profile, default platen-notary
set -eu

cd "$(dirname "$0")"
SIGN=1
NOTARIZE=1
for arg in "$@"; do
	case "$arg" in
	--no-sign) SIGN=0 NOTARIZE=0 ;;
	--no-notarize) NOTARIZE=0 ;;
	*) echo "unknown option $arg" >&2; exit 2 ;;
	esac
done

BUNDLE_ID=${PLATEN_BUNDLE_ID:-io.github.tim-riep.platen}
SIGN_APP=${PLATEN_SIGN_APP:-Developer ID Application}
SIGN_PKG=${PLATEN_SIGN_PKG:-Developer ID Installer}
NOTARY_PROFILE=${PLATEN_NOTARY_PROFILE:-platen-notary}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)}
# CFBundleShortVersionString must be numeric; other builds get 0.0.0.
SHORT=$(echo "$VERSION" | sed -n 's/^\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p')
SHORT=${SHORT:-0.0.0}
BUILD=$(git rev-list --count HEAD 2>/dev/null || echo 1)

OUT=build
APP=$OUT/Platen.app
rm -rf "$OUT"
mkdir -p "$OUT"

echo "==> Building $VERSION ($SHORT, build $BUILD)"
for arch in arm64 x86_64; do
	swift build -c release --product Platen --triple "$arch-apple-macosx14.0"
done
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
lipo -create -output "$APP/Contents/MacOS/Platen" \
	.build/arm64-apple-macosx/release/Platen .build/x86_64-apple-macosx/release/Platen
sed -e "s/@BUNDLE_ID@/$BUNDLE_ID/" -e "s/@VERSION@/$SHORT/" -e "s/@BUILD@/$BUILD/" \
	Resources/Info.plist >"$APP/Contents/Info.plist"
plutil -lint "$APP/Contents/Info.plist" >/dev/null
cp Resources/platen.ppd "$APP/Contents/Resources/"
cp ../../LICENSE "$APP/Contents/Resources/LICENSE.txt"
swift scripts/make-icon.swift "$OUT/AppIcon.iconset"
iconutil -c icns -o "$APP/Contents/Resources/AppIcon.icns" "$OUT/AppIcon.iconset"

if [ $SIGN = 1 ]; then
	echo "==> Signing the app"
	codesign --force --options runtime --timestamp -s "$SIGN_APP" "$APP"
	codesign --verify --strict --verbose=2 "$APP"
fi

echo "==> Building the installer"
mkdir -p "$OUT/root/Applications" "$OUT/scripts"
ditto "$APP" "$OUT/root/Applications/Platen.app"
cp scripts/preinstall scripts/postinstall "$OUT/scripts/"
pkgbuild --analyze --root "$OUT/root" "$OUT/component.plist"
# Always install to /Applications, never into a moved copy of the app.
plutil -replace 0.BundleIsRelocatable -bool NO "$OUT/component.plist"
pkgbuild --root "$OUT/root" --component-plist "$OUT/component.plist" --scripts "$OUT/scripts" \
	--identifier "$BUNDLE_ID.pkg" --version "$SHORT" --install-location / "$OUT/Platen-component.pkg"
sed -e "s/@BUNDLE_ID@/$BUNDLE_ID/g" -e "s/@VERSION@/$SHORT/g" Resources/distribution.xml >"$OUT/distribution.xml"
PKG=$OUT/Platen-$VERSION.pkg
if [ $SIGN = 1 ]; then
	productbuild --distribution "$OUT/distribution.xml" --package-path "$OUT" --sign "$SIGN_PKG" --timestamp "$PKG"
	pkgutil --check-signature "$PKG"
else
	productbuild --distribution "$OUT/distribution.xml" --package-path "$OUT" "$PKG"
fi

if [ $NOTARIZE = 1 ]; then
	if [ -n "${NOTARY_KEY_PATH:-}" ] && [ -n "${NOTARY_KEY_ID:-}" ] && [ -n "${NOTARY_ISSUER_ID:-}" ]; then
		echo "==> Notarizing (API key)"
		set -- --key "$NOTARY_KEY_PATH" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER_ID"
	else
		echo "==> Notarizing (profile $NOTARY_PROFILE)"
		set -- --keychain-profile "$NOTARY_PROFILE"
	fi
	result=$(xcrun notarytool submit "$PKG" --wait --output-format json "$@") || true
	echo "$result"
	case "$result" in
	*'"status":"Accepted"'*) ;;
	*)
		id=$(printf '%s' "$result" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
		[ -z "$id" ] || xcrun notarytool log "$id" "$@" >&2
		echo "notarization of $PKG failed" >&2
		exit 1
		;;
	esac
	# Fetching the ticket from Apple's server fails now and then (the
	# connection drops). The package is notarized either way: without a
	# stapled ticket, Gatekeeper asks Apple online when it is opened.
	stapled=0
	for try in 1 2 3 4 5; do
		if xcrun stapler staple "$PKG"; then
			stapled=1
			break
		fi
		echo "stapling failed (attempt $try)" >&2
		[ "$try" = 5 ] || sleep 30
	done
	if [ $stapled = 1 ]; then
		spctl --assess --type install --verbose=2 "$PKG"
	else
		msg="the notarization ticket could not be stapled; $PKG is notarized, Gatekeeper checks it online"
		if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::warning::$msg"; else echo "warning: $msg" >&2; fi
	fi
fi

rm -rf "$OUT/root" "$OUT/scripts" "$OUT/AppIcon.iconset" "$OUT/component.plist" "$OUT/distribution.xml" "$OUT/Platen-component.pkg"
echo "==> $PKG"
