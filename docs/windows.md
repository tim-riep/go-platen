# Windows app

Platen for Windows installs the printers of a platen server as Windows
printers. Any application can print to them; the jobs reach the server as
PDF. It is the Windows counterpart of the [macOS app](macos.md).

## Requirements

- Windows 10 or 11, 64-bit (x64; ARM devices run it emulated)
- The computer is connected to the NetBird network of the platen server
- The computer may print: it is a panel user, or it belongs to a NetBird
  user that is not blocked and the server syncs NetBird users (see
  [macos.md](macos.md#requirements) and [panel.md](panel.md))
- Administrator rights only to install. Users need none: the Platen service
  does everything that needs them.

## Use

1. An administrator runs `Platen-<version>-setup.exe`. It installs to
   `C:\Program Files\Platen`, starts the Platen service and can open Platen
   at the end.
2. On the settings page in the browser, enter the server's NetBird name or
   IP and choose **Connect & install printers**. Any signed-in user can do
   this, unless the administrator locked the server (below).
3. The printers appear in every print dialog under their platen names, for
   all users of the computer. If a name is already used by another printer,
   " (platen)" is appended.

The Platen icon in the notification area of every user shows the
connection and waiting jobs; its menu opens the settings page or syncs the
printers, and **Hide icon** removes it until the next sign-in (printing goes
on). The Start menu entry opens the page. The service checks the server
every five minutes and adds, renames or removes printers. **Remove printers
& disconnect** deletes all printers Platen installed.

Only printers whose backend takes PDF are installed; `raw` printers (e.g.
ZPL) are left out.

### Managed rollout

The installer takes options for software distribution (Intune, GPO, …):

```
Platen-<version>-setup.exe /S /SERVER=printserver.netbird.cloud /LOCK
```

- `/S` installs silently.
- `/SERVER=<host>` sets the print server, so the printers appear without
  anyone opening Platen.
- `/LOCK` keeps users from changing or removing the server; the page shows
  it as set by the administrator. Without it, every signed-in user may
  change the server for the whole computer, which is fine on personal
  computers but not on shared ones.

`Platen.exe set-server <host> [-lock]`, run as administrator, changes the
server later.

## How it works

```
Application ── Windows printer (Microsoft Print To PDF driver)
             ── local port \\.\pipe\platen-<id> ──▶ Platen service ── spool ──▶ POST /client/v1/printers/<id>/jobs
                                                         ▲
                     tray icon / settings page (users) ──┘
```

- **Service.** The Platen service (`Platen.exe service`, LocalSystem,
  automatic start, restarted after failures) runs for all users. It
  manages the printers, receives and spools the jobs, talks to the server
  and serves the settings page. Its data (configuration, spool, log) is in
  `%ProgramData%\Platen`, which only SYSTEM and administrators may open,
  since the spool holds every user's documents.
- **Printers.** Each platen printer becomes a printer with the built-in
  "Microsoft Print To PDF" driver on a local port that is the named pipe
  `\\.\pipe\platen-<printer ID>`. The driver renders every document to
  PDF and the Windows spooler writes it into the pipe. The port name marks
  the printers Platen manages. The paper size of the platen printer is the
  default. The service uses the PrintManagement PowerShell cmdlets
  (`Add-PrinterPort`, `Add-Printer`, `Rename-Printer`, `Remove-Printer`) and
  checks every printer ID, name and paper size from the server before. The
  installer turns the "Microsoft Print to PDF" Windows feature on if it is
  off.
- **Receiver.** The service listens on the pipe of every printer it
  manages. Named pipes do not use network ports, so no other service can be
  in the way, and their names are Platen's own. Only the spooler, the
  signed-in users and administrators may open them; access over the
  network is refused. The job title is the document name of the job in the
  printer's Windows queue. If the service is stopped, Windows keeps the jobs
  in its queue and retries.
- **Spool and server.** Jobs wait in the spool until the server accepted
  them, with the same retries and idempotency keys as on macOS (see
  [macos.md](macos.md#how-it-works)), through the client API (`/client/v1`).
- **Settings page.** The service serves it on a random port of `127.0.0.1`
  under a random path. The path is in `%ProgramData%\Platen\public\ui.json`,
  which signed-in users may read; the tray icon opens the page from there.
  Changing requests need the `X-Platen-Desktop` header and the exact host,
  which keeps web pages out.

The log is `%ProgramData%\Platen\platen.log`.

## Building

`desktop/windows/build.sh` builds on macOS or Linux with Go and NSIS
(`brew install makensis`, `apt install nsis`):

```bash
desktop/windows/build.sh --no-sign   # unsigned, for tests
desktop/windows/build.sh             # signed
```

It embeds the icon, manifest and version information with go-winres (0BSD,
run with `go run`, not a dependency), builds `Platen.exe` for windows/amd64
without cgo and writes `Platen-<version>-setup.exe` to
`desktop/windows/build`. The icon `cmd/platen-desktop/platen.ico` is drawn by
`go run ./desktop/windows/mkicon cmd/platen-desktop/platen.ico`.

### Signing

Signing uses the Certum code signing certificate in the SimplySign cloud
through [ssign](https://github.com/Le-Syl21/ssign) (`cargo install ssign
--version 0.1.8 --locked`) and checks each signature with `osslsigncode`.
`Platen.exe`, the uninstaller and the installer are signed. ssign needs
`CERTUM_EMAIL` and `CERTUM_OTP` (the TOTP seed of SimplySign, base32) in the
environment.

CI builds an unsigned installer on every push; it never signs anything. A
version tag (`v*`) starts the release workflow, which builds the signed
installer and attaches it to the GitHub release. For that, the repository
needs the secrets `CERTUM_EMAIL` and `CERTUM_OTP`:

```bash
gh secret set CERTUM_EMAIL --repo tim-riep/go-platen
gh secret set CERTUM_OTP --repo tim-riep/go-platen
```

The workflow **Signing check** (`gh workflow run signing-check.yml`) tests
them without publishing anything.

### Tests

`go test ./internal/desktop` covers the receiving of documents, the spool, the printer
planning, the generated PowerShell and the whole path from a print job to
the server. On Windows, `PLATEN_WINDOWS_E2E=1` adds two tests: one installs
real printers, prints on one and removes them; the other installs the
service with a preset, locked server, waits until it installed the
printers, prints and uninstalls it. CI runs them on a Windows runner. They
change the computer's printers and services and need administrator rights.

## Uninstall

Uninstall Platen in Settings → Apps (administrator). The uninstaller stops
and removes the service, removes all printers Platen installed and deletes
`%ProgramData%\Platen`, including jobs that were not sent yet.
