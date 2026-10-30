#!/bin/sh
# Fails if a module licensed under MPL-2.0 is compiled into platen without
# being listed in .github/licenses/mpl-approved.txt. Requires go-licenses v2.
set -eu
cd "$(dirname "$0")/.."
approved=$(mktemp)
found=$(mktemp)
trap 'rm -f "$approved" "$found"' EXIT
grep -v '^#' .github/licenses/mpl-approved.txt | sed '/^$/d' | sort -u > "$approved"
GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}" go-licenses report ./... 2>/dev/null |
	awk -F, '$3 == "MPL-2.0" {print $1}' | sort -u > "$found"
unapproved=$(comm -23 "$found" "$approved")
if [ -n "$unapproved" ]; then
	echo "MPL-2.0 modules not approved in .github/licenses/mpl-approved.txt:" >&2
	echo "$unapproved" | sed 's/^/  /' >&2
	exit 1
fi
echo "MPL-2.0 modules: all approved"
sed 's/^/  /' "$found"
