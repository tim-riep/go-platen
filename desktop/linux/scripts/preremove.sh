#!/bin/sh
set -e
# On removal (not on upgrade) the printers go with the service.
if [ "$1" = remove ]; then
	if [ -d /run/systemd/system ]; then
		systemctl stop platen-desktop.service || true
		systemctl disable platen-desktop.service >/dev/null 2>&1 || true
	fi
	/usr/bin/platen-desktop remove-printers || true
fi
exit 0
