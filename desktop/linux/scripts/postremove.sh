#!/bin/sh
set -e
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
# Purging deletes configuration, log and unsent jobs.
if [ "$1" = purge ]; then
	rm -rf /var/lib/platen-desktop
	if getent passwd platen >/dev/null; then
		deluser --quiet --system platen >/dev/null 2>&1 || true
	fi
fi
exit 0
