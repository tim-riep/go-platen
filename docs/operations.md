# Operations

## Build

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --always)" -o platen ./cmd/platen
```

`linux/arm64` works the same way. CI builds both.

## Install

```bash
sudo install -m 0755 platen /usr/local/bin/platen
sudo useradd --system --home /var/lib/platen --shell /usr/sbin/nologin platen
sudo install -d -m 0750 -o root -g platen /etc/platen
```

Example configurations: [`deploy/config/`](../deploy/config).
systemd units: [`deploy/systemd/`](../deploy/systemd). To run platen in
containers instead, see [docker.md](docker.md).

Secret files belong to the `platen` user with mode `0600`:

```bash
sudo install -m 0600 -o platen -g platen /dev/stdin /etc/platen/setup-key
```

## Server

1. Generate the admin token and put the printed hash into
   `server.admin_token_hash`:

   ```bash
   platen hash-token -generate
   ```

   Store the token itself for the admin CLI (`admin_token_file` or
   `PLATEN_ADMIN_TOKEN`). The server only knows the hash.
2. Write `/etc/platen/server.toml` (see `deploy/config/server.toml`) and the
   NetBird setup key file.
3. Start the server:

   ```bash
   sudo systemctl enable --now platen-server
   ```

The server listens only inside the mesh, on `netbird.port` of its mesh
address. `GET /v1/health` (no authentication) shows whether it is up.

### Configuration reference (server)

| Key | Default | Meaning |
| --- | --- | --- |
| `log_level` | `info` | `debug`, `info`, `warn`, `error` |
| `netbird.management_url` | NetBird default | management server URL |
| `netbird.config_path` | required | persistent NetBird identity |
| `netbird.setup_key_file` | | setup key file (or `PLATEN_NETBIRD_SETUP_KEY`) |
| `netbird.hostname` | `platen-server` | device name and DNS label in the mesh |
| `netbird.port` | `80` | listen port in the mesh |
| `netbird.log_level` | `warn` | NetBird log level |
| `server.database` | required | SQLite database file |
| `server.spool_dir` | required | document directory |
| `server.admin_token_hash` | | `sha256:<hex>`; empty disables the admin API |
| `server.max_document_bytes` | 64 MiB | document size limit |
| `server.job_retention` | `168h` | how long finished jobs and their documents are kept |
| `server.cleanup_interval` | `10m` | cleanup interval |
| `server.lease_duration` | `60s` | lease duration; progress extends it |
| `server.max_attempts` | `3` | attempts before a job fails |
| `server.enrollment_token_ttl` | `24h` | default lifetime of enrollment tokens |
| `local_worker.enabled` | `false` | run the built-in worker (ID `local`) |
| `local_worker.name` | `built-in worker` | its display name |
| `local_worker.journal_dir` | required if enabled | its journal |
| `local_worker.temp_dir` | system temp dir | its working files |
| `panel.enabled` | `true` | web panel on the mesh port, see [panel.md](panel.md) |
| `panel.allowed_hosts` | | further host names the panel may be opened by |

File paths for secrets may contain environment variables, e.g.
`$CREDENTIALS_DIRECTORY/setup-key` with systemd credentials.

## Setting up printers, workers and applications

All commands below run `platen admin -config admin.toml ...` on a peer in
the `platen-admins` group. Output is JSON.

```bash
# A remote site
platen admin -config admin.toml worker create site-hamburg -name "Hamburg warehouse" -location Hamburg
platen admin -config admin.toml worker enroll site-hamburg -ttl 24h     # prints the enrollment token

# Printers of that site, addressed in the site's local network
platen admin -config admin.toml printer create zebra-1 -worker site-hamburg -address 192.168.10.20 -backend raw
platen admin -config admin.toml printer create office-1 -worker site-hamburg -address 192.168.10.21 \
    -backend pdf-to-raster -paper A4 -dpi 600 -option dither=threshold

# A driverless inkjet (IPP Everywhere, AirPrint), port 631 if omitted
platen admin -config admin.toml printer create inkjet-1 -worker site-hamburg -address 192.168.10.22 -backend ipp -dpi 300

# A printer at the server, served by the built-in worker
platen admin -config admin.toml printer create front-desk -worker local -address 10.0.0.30 -backend raw

# The ERP
platen admin -config admin.toml client create erp -name "Web ERP" -printers zebra-1,office-1   # prints the app token
```

## Worker

1. Put the enrollment token into `/etc/platen/enrollment-token` (or
   `PLATEN_ENROLLMENT_TOKEN`) and the NetBird setup key into
   `/etc/platen/setup-key`.
2. Write `/etc/platen/worker.toml` (see `deploy/config/worker.toml`).
3. Start the worker:

   ```bash
   sudo systemctl enable --now platen-worker
   ```

On the first start the worker joins the mesh, exchanges the enrollment token
for its permanent credential (stored in `worker.credential_file`, mode 0600)
and is bound to its NetBird peer. Both secret files can then be deleted.

### Configuration reference (worker)

| Key | Meaning |
| --- | --- |
| `netbird.*` | as for the server; `port` is not used |
| `worker.server_url` | the server in the mesh, e.g. `http://platen-server.netbird.cloud` |
| `worker.credential_file` | where the permanent credential is stored |
| `worker.journal_dir` | the delivery journal |
| `worker.enrollment_token_file` | token for the first start |
| `worker.temp_dir` | working files (downloaded and rendered documents) |

Printers are not configured on the worker; it receives them from the server
and refreshes them with every heartbeat (30 s).

## Printer options

Set with `-option key=value` on `printer create`/`update`:

| Backend | Option | Values |
| --- | --- | --- |
| `pdf-to-raster`, `brother-raster` | `dither` | `threshold`, `halftone`, `floyd-steinberg`; default `halftone` for `brother-raster`, `threshold` for `pdf-to-raster` |
| | `threshold` | `1`–`255` (default `128`) |
| | `rotate` | `auto` (default), `none` |
| `brother-raster` | `source` | `tray` (default), `auto`, `manual` (manual feed slot) |
| | `media` | `plain` (default), `thick` |
| | `toner_save` | `off` (default), `on` |

| `ipp` | `format` | `auto` (default: PDF if the printer accepts it, else PWG Raster), `pdf`, `pwg-raster` |
| | `color` | `auto` (default: the printer's default for PDF, color raster if supported), `color`, `monochrome` |
| | `sides` | `one-sided`, `two-sided-long-edge`, `two-sided-short-edge`; default: the printer's default |
| | `quality` | `draft`, `normal`, `high`; default: the printer's default |
| | `rotate` | `auto` (default), `none`; for PWG Raster |
| | `path` | the printer's IPP resource, default `/ipp/print` |
| | `wait` | how long to wait for the job to complete, e.g. `10m` (default), `0` to stop once the printer accepted it |
| | `insecure-tls` | `true` accepts the printer's self-signed certificate for `ipps://` addresses |

The address of an `ipp` printer is `host[:port]` (port 631 if omitted) or a
full `ipp://` or `ipps://` URI, e.g. `ipps://canon.local/ipp/print`. The
printer's `resolution_dpi` is the highest raster resolution used; most
inkjets take 300 or 600 dpi. Printers that take only Apple's URF (some
AirPrint-only models) are not supported. Two-sided PWG Raster requires a
printer that does not turn back sides (`pwg-raster-document-sheet-back`
`normal`); otherwise use `format=pdf` or one-sided printing. Paper sizes are
sent as `media` only if the printer lists them.

`brother-raster` supports A4 at 300, 600 and 1200 dpi (`-dpi 1200` is the
driver's "HQ 1200") and Letter and A5 at 600 dpi. See
[brother-raster-format.md](brother-raster-format.md) for what the captures
cover.

## Monitoring and logs

platen logs to stderr (journald under systemd) in `slog` text format.
Useful events: `job queued`, `job leased`, `job done`, `job failed`,
`lease expired`, `worker enrolled`, `worker request from unexpected peer`,
`netbird management connection lost`.

`platen admin worker list` shows each worker's last heartbeat and version;
`platen admin printer list` shows the last reported reachability.

## Backup

Stop the server or use SQLite's online backup, and back up the database and
the spool directory together. Back up `netbird.config_path` to keep the
server's NetBird identity.

## Failure handling

| Situation | Behavior |
| --- | --- |
| Printer unreachable | The job fails as retryable and is requeued until `max_attempts`. |
| Connection lost while sending | Reported as uncertain; the job fails with a note and is not retried. Check the printer and resubmit if needed. |
| Worker crash while sending | After the restart the job is reported as uncertain, not printed again. |
| Worker crash before sending | The lease expires and the job is retried. |
| Server unreachable | The worker retries with exponential backoff; prints in progress complete and are reported later. |
| ERP retries a submission | The same `Idempotency-Key` returns the existing job. |
