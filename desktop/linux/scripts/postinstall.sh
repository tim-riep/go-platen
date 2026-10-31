#!/bin/sh
set -e
# The service user is a member of the CUPS system group lpadmin, so it can
# change printers and see the names of all jobs without root.
if ! getent passwd platen >/dev/null; then
	adduser --system --group --home /var/lib/platen-desktop --no-create-home --quiet platen
fi
if getent group lpadmin >/dev/null; then
	usermod -a -G lpadmin platen
fi
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload
	systemctl enable platen-desktop.service >/dev/null 2>&1 || true
	systemctl restart platen-desktop.service || true
fi
exit 0
