# Linux app

Platen for Linux installs the printers of a platen server as CUPS printers.
Any application can print to them; the jobs reach the server as PDF. It
works like the [Windows app](windows.md): a system service does the work,
and every user has a tray icon and a settings page in the browser.

## Requirements

- Debian 12 or later, Ubuntu 22.04 or later, or a derivative, amd64 or
  arm64, with systemd
- CUPS and cups-filters (dependencies of the package)
- The computer is connected to the NetBird network of the platen server
- The computer may print: it is a panel user, or it belongs to a NetBird
  user that is not blocked and the server syncs NetBird users (see
  [macos.md](macos.md#requirements) and [panel.md](panel.md))
- Root only to install. Users need no rights.

## Use

1. Install the package:

   ```bash
   sudo apt install ./platen-desktop_<version>_amd64.deb
   ```

   It starts the service `platen-desktop`.
2. Open **Platen** from the application menu. The settings page opens in the
   browser. Enter the server's NetBird name or IP and choose **Connect &
   install printers**. Any signed-in user can do this, unless the server is
   locked (below).
3. The printers appear in every print dialog under their platen names, for
   all users of the computer.

Each user's session starts the tray icon (XDG autostart). It shows the
connection and waiting jobs; its menu opens the settings page or syncs the
printers. Desktops without a tray for StatusNotifierItem icons (GNOME
without the AppIndicator extension, for example) show no icon; the menu
entry still opens the page. The service checks the server every five
minutes and adds, renames or removes printers. **Remove printers &
disconnect** deletes all printers Platen installed.

Only printers whose backend takes PDF are installed; `raw` printers (e.g.
ZPL) are left out.

### Managed rollout

```bash
sudo platen-desktop set-server printserver.netbird.cloud -lock
```

sets the print server for the service and restarts it. With `-lock`, users
may not change or remove it; without, every signed-in user may change the
server for the whole computer.

## How it works

```
Application ── CUPS queue platen_<id> (platen.ppd: PDF)
             ── socket://127.0.0.1:<port> ──▶ platen-desktop service ── spool ──▶ POST /client/v1/printers/<id>/jobs
                                                ▲
                  tray icon / settings page ────┘
```

- **Service.** `platen-desktop.service` runs `platen-desktop service` as
  the system user `platen`, which the package creates and adds to the CUPS
  system group `lpadmin`, so it can add and remove queues and see job names
  without root. Its data (configuration, spool, log) is in
  `/var/lib/platen-desktop` (only for that user). The unit restricts the
  service (no new privileges, read-only system, no access to home
  directories).
- **Printers.** Each platen printer becomes a CUPS queue `platen_<printer
  ID>` with the PPD of the macOS app (`/usr/share/platen-desktop/platen.ppd`),
  which makes CUPS convert every document to PDF (with cups-filters), and the
  device URI `socket://127.0.0.1:<port>`, CUPS' own AppSocket backend. The
  queues are not shared and retry their jobs while the service is not
  running. The paper size of the platen printer is the default.
- **Receiver.** As on Windows, the service listens on `127.0.0.1` only,
  with a free port per printer that it picks itself; if a port is taken when
  the service starts, it moves the queue to another one. The job title comes
  from CUPS over IPP (Get-Jobs) through CUPS' local socket.
- **Spool and server.** Jobs wait in the spool until the server accepted
  them, with the same retries and idempotency keys as on macOS (see
  [macos.md](macos.md#how-it-works)), through the client API (`/client/v1`).
- **Settings page.** The service serves it on a random port of `127.0.0.1`
  under a random path, which is in `/run/platen-desktop/ui.json` for the
  users' tray icons. Changing requests need the `X-Platen-Desktop` header and
  the exact host, which keeps web pages out.

Logs: `journalctl -u platen-desktop` and `/var/lib/platen-desktop/platen.log`.

## Building

```bash
desktop/linux/build.sh
```

builds `platen-desktop` for linux/amd64 and linux/arm64 without cgo and the
packages `platen-desktop_<version>_<arch>.deb` in `desktop/linux/build`
with [nfpm](https://github.com/goreleaser/nfpm) (MIT, run with `go run`). The
version comes from `git describe`; builds after a tag sort above it
(`1.2.3+4.gabc`), prereleases below (`1.2.3~rc1`). The packages are not
signed.

`go test ./internal/desktop` covers the CUPS output parsing, the IPP
request, the queue planning and the path from a print job to the server. On
Linux, `PLATEN_LINUX_E2E=1` adds a test with real CUPS queues, and
`PLATEN_LINUX_DEB=<package>` one that installs the package with systemd,
prints through the service and purges it; both need root. CI runs them on
Ubuntu.

## Uninstall

```bash
sudo apt purge platen-desktop
```

removes the printers, the service, the user `platen` and
`/var/lib/platen-desktop`, including jobs that were not sent yet. `apt
remove` keeps the data.
