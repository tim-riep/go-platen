# macOS app

Platen for macOS is a menu bar app that installs the printers of a platen
server as local printers. Any app can then print to them: the print dialog
shows them like any other printer, and the jobs reach the server as PDF.

## Requirements

- macOS 14 or later, Apple silicon or Intel
- The Mac is connected to the NetBird network of the platen server
- The Mac may print. That is the case if
  - it is a panel user (admin or viewer, see [panel.md](panel.md)), or
  - it belongs to a NetBird user that is not blocked and the server has a
    NetBird API token for the user sync. A phone or laptop of the same
    user then needs no panel entry.

  Devices that joined NetBird with a setup key belong to no user and need a
  panel role.

## Use

1. Install `Platen-<version>.pkg`. Platen starts after the installation and
   shows its window.
2. Enter the server's NetBird name or IP (for example
   `platen-server.netbird.cloud`) and choose **Connect & Install Printers**.
3. The printers appear in every print dialog as their platen names. Platen
   opens at login from now on (it can be turned off in the window).

The menu bar icon shows the connection, waiting jobs and **Sync Printers
Now**. Platen checks the server every five minutes: new printers are added,
renamed ones are updated and removed ones are deleted. **Remove Printers &
Disconnect** deletes all printers Platen installed.

Only printers whose backend takes PDF are installed (`pdf-to-raster`,
`brother-raster`); `raw` printers (e.g. ZPL) are left out.

## How it works

```
App ── print dialog ── CUPS queue platen_<id> ── PDF via LPD ──▶ Platen (127.0.0.1:6515)
                                                                  │ spool
                                                                  ▼
                                    platen server  POST /client/v1/printers/<id>/jobs
```

- **Queues.** For every printer Platen creates a CUPS queue `platen_<id>`
  with the device URI `lpd://127.0.0.1:6515/<id>?reserve=none` and its own
  PPD (`desktop/macos/Resources/platen.ppd`). The PPD makes CUPS deliver PDF
  and render copies into the document. The paper size of the platen printer
  becomes the default page size. Queues are not shared and retry their jobs
  while Platen is not running. Admin users create them without a password;
  other users are asked for an administrator once.
- **Receiver.** Platen accepts the jobs with the LPD protocol (RFC 1179) on
  the loopback interface only. The job title comes from the control file.
- **Spool.** Jobs are stored in `~/Library/Application Support/Platen/spool`
  until the server accepted them. If the server is unreachable or does not
  know the device yet, Platen retries with a growing interval (up to five
  minutes) for 24 hours. Jobs the server refuses for good (unknown printer,
  unsupported format, too large) are shown in the window with the reason.
  Retries use an idempotency key, so a job is never queued twice.
- **Server API.** The client API (`/client/v1`, see
  [api/openapi.yaml](../api/openapi.yaml)) needs no token: like the panel,
  the server identifies the device by its NetBird peer, checks the `Host`
  header and requires the `X-Platen-Client` header. Jobs record
  `device:<NetBird IP>` as their submitter.
- **HTTP.** The server speaks HTTP inside the mesh, whose WireGuard tunnels
  encrypt the traffic, so the app allows plain HTTP connections (App
  Transport Security exception).

Logs go to the unified log:

```bash
log stream --predicate 'subsystem == "io.github.tim-riep.platen"'
```

## Building

The app is a Swift package in `desktop/macos` and builds with the Command
Line Tools (Xcode is not needed).

```bash
cd desktop/macos
swift run PlatenChecks        # checks of the protocol, spool and queue logic
./build.sh --no-sign          # unsigned app and package for local tests
./build.sh --no-notarize      # signed app and package
./build.sh                    # signed, notarized and stapled
```

`build.sh` builds arm64 and x86_64 and joins them with `lipo`, writes the app
bundle (`Info.plist`, PPD, icon) and the installer package to
`desktop/macos/build`. The version comes from `git describe`; set `VERSION`
to override it.

### Signing and notarization

Signing uses the identities `Developer ID Application` (app, with the
hardened runtime and a secure timestamp) and `Developer ID Installer`
(package) from the keychain; `PLATEN_SIGN_APP` and `PLATEN_SIGN_PKG` select
others (name or SHA-1). Notarization uses an App Store Connect API key if
`NOTARY_KEY_PATH`, `NOTARY_KEY_ID` and `NOTARY_ISSUER_ID` are set, otherwise
the notarytool keychain profile `platen-notary` (`PLATEN_NOTARY_PROFILE`).
Create the profile once, either with an app-specific password of the Apple
ID:

```bash
xcrun notarytool store-credentials platen-notary --apple-id <apple-id> --team-id <team-id>
```

or with an API key:

```bash
xcrun notarytool store-credentials platen-notary --key AuthKey_<key-id>.p8 --key-id <key-id> --issuer <issuer-id>
```

`build.sh` then submits the package, waits for the result (and prints the
notary log if it is rejected), staples the ticket and checks it with
`spctl`.

The bundle identifier is `io.github.tim-riep.platen`; set
`PLATEN_BUNDLE_ID` to use another one.

### CI and releases

CI builds and checks the app on every push and keeps the unsigned package
as an artifact; it never signs anything. A version tag (`v*`) starts the
release workflow, which signs and notarizes the package, like the pdx
project, and attaches it to the GitHub release. It uses these repository
secrets:

| Secret | Content |
| --- | --- |
| `MACOS_CERT_P12` | Both Developer ID certificates (Application and Installer) with their private keys, exported from Keychain Access as one .p12, base64 encoded |
| `MACOS_CERT_PASSWORD` | The password of the .p12 |
| `NOTARY_KEY_P8` | The content of the App Store Connect API key file `AuthKey_<key-id>.p8` |
| `NOTARY_KEY_ID` | The key's ID |
| `NOTARY_ISSUER_ID` | The Issuer ID (a UUID, above the key table in App Store Connect) |

```bash
base64 -i certificates.p12 | gh secret set MACOS_CERT_P12 --repo tim-riep/go-platen
gh secret set MACOS_CERT_PASSWORD --repo tim-riep/go-platen
gh secret set NOTARY_KEY_P8 --repo tim-riep/go-platen < AuthKey_<key-id>.p8
gh secret set NOTARY_KEY_ID --repo tim-riep/go-platen
gh secret set NOTARY_ISSUER_ID --repo tim-riep/go-platen
```

`desktop/macos/ci-keychain.sh` imports the certificates into a temporary
keychain, masks the certificate names and fingerprints in the logs and
passes the identities to `build.sh`; the keychain is deleted at the end.
The workflow **Signing check** (`gh workflow run signing-check.yml`) tests
the macOS and Windows secrets without publishing anything.

## Uninstall

Choose **Remove Printers & Disconnect**, quit Platen and delete
`/Applications/Platen.app` and `~/Library/Application Support/Platen`.
