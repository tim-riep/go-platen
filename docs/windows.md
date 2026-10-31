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
- Administrator rights to install, and to add or remove printers (Windows
  asks through UAC)

## Use

1. Run `Platen-<version>-setup.exe`. It installs to `C:\Program Files\Platen`
   and can start Platen at the end.
2. Platen opens its settings page in the browser. Enter the server's NetBird
   name or IP and choose **Connect & install printers**. Windows asks for
   administrator permission to add the printers.
3. The printers appear in every print dialog under their platen names. If a
   name is already used by another printer, " (platen)" is appended.
   Platen starts at sign-in from now on (it can be turned off on the page).

The Platen icon in the notification area shows the connection and waiting
jobs; its menu opens the settings page, syncs the printers or quits Platen.
Starting Platen again also opens the page. Platen checks the server every
five minutes and adds, renames or removes printers; Windows asks for
permission only when something changes. **Remove printers & disconnect**
deletes all printers Platen installed.

Only printers whose backend takes PDF are installed; `raw` printers (e.g.
ZPL) are left out.

## How it works

```
Application ── Windows printer (Microsoft Print To PDF driver)
             ── local port \\.\pipe\platen-<id> ──▶ Platen ── spool ──▶ POST /client/v1/printers/<id>/jobs
```

- **Printers.** Each platen printer becomes a printer with the built-in
  "Microsoft Print To PDF" driver on a local port that is the named pipe
  `\\.\pipe\platen-<printer ID>`. The driver renders every document to
  PDF and the Windows spooler writes it into the pipe. The port name marks
  the printers Platen manages. The paper size of the platen printer is the
  default. The installer turns the "Microsoft Print to PDF" Windows feature
  on if it is off.
- **Changes.** Platen uses the PrintManagement PowerShell cmdlets
  (`Add-PrinterPort`, `Add-Printer`, `Rename-Printer`, `Remove-Printer`).
  Without administrator rights it starts itself elevated with the
  `apply-printers` command for the change. The elevated step reads only a
  list of changes, checks that every printer ID, name and paper size is
  valid, and builds the PowerShell script itself.
- **Receiver.** Platen listens on the pipe of every printer it manages.
  Named pipes do not use network ports, so no other service can be in the
  way, and their names are Platen's own. Only the spooler (SYSTEM) and the
  signed-in user may open them; access over the network is refused. The job
  title is the document name of the job in the printer's Windows queue.
  While Platen is not running, Windows keeps the jobs in its queue and
  retries. With several users signed in at once, the first one's Platen
  receives the jobs (they are sent as this device either way); the other
  Platen says so on its page.
- **Spool and server.** Jobs wait in `%LOCALAPPDATA%\Platen\spool` until the
  server accepted them, with the same retries and idempotency keys as on
  macOS (see [macos.md](macos.md#how-it-works)), through the client API
  (`/client/v1`).
- **Settings page.** It is served on a random port of `127.0.0.1` under a
  random path that only the signed-in user can read (in
  `%LOCALAPPDATA%\Platen\ui.json`). Changing requests need the
  `X-Platen-Desktop` header and the exact host, which keeps other web pages
  out.

The log is `%LOCALAPPDATA%\Platen\platen.log`.

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

CI builds an unsigned installer on every push and a signed one for version
tags (`v*`). For that, the repository needs the secrets `CERTUM_EMAIL` and
`CERTUM_OTP`:

```bash
gh secret set CERTUM_EMAIL --repo tim-riep/go-platen
gh secret set CERTUM_OTP --repo tim-riep/go-platen
```

### Tests

`go test ./internal/desktop` covers the receiving of documents, the spool, the printer
planning, the generated PowerShell and the whole path from a print job to
the server. On Windows, `PLATEN_WINDOWS_E2E=1` adds a test that installs
real printers, prints on one and removes them again; CI runs it on a
Windows runner. It changes the computer's printers and needs administrator
rights.

## Uninstall

Uninstall Platen in Settings → Apps. The uninstaller removes all printers
Platen installed and the sign-in entry of the user who uninstalls; the data
in `%LOCALAPPDATA%\Platen` stays.
