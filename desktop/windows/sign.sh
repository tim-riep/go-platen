#!/usr/bin/env bash
# Signs a Windows executable with the Certum code signing certificate
# (SimplySign cloud) through ssign and verifies the signature.
#
#   desktop/windows/sign.sh <file.exe>
#
# Environment: CERTUM_EMAIL, CERTUM_OTP (TOTP seed, base32; ssign derives
# the one-time code), optional SSIGN (path to ssign). ssign's output names
# the certificate holder and is kept out of the log.
set -euo pipefail
[ $# -eq 1 ] || { echo "usage: $0 <file.exe>" >&2; exit 2; }
: "${CERTUM_EMAIL:?CERTUM_EMAIL is not set}"
: "${CERTUM_OTP:?CERTUM_OTP is not set}"
ssign=${SSIGN:-ssign}
log=$(mktemp)
trap 'rm -f "$log"' EXIT

# A one-time code is accepted once per 30 second window, so a failed
# attempt (e.g. a second file in the same window) waits for the next one.
for try in 1 2 3; do
	if "$ssign" -n Platen -u https://github.com/tim-riep/go-platen "$1" >"$log" 2>&1; then
		break
	fi
	echo "sign: attempt $try failed" >&2
	if [ "$try" = 3 ]; then
		grep -viE 'subject|issuer|cn=' "$log" >&2 || true
		exit 1
	fi
	sleep 35
done

ca=/etc/ssl/certs/ca-certificates.crt
[ -f "$ca" ] || ca="$(brew --prefix 2>/dev/null)/etc/ca-certificates/cert.pem"
if ! osslsigncode verify -CAfile "$ca" -TSA-CAfile "$ca" -in "$1" >/dev/null 2>&1; then
	echo "sign: the signature of ${1##*/} is not valid" >&2
	exit 1
fi
echo "sign: ${1##*/} signed"
