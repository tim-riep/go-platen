# platen

platen is a print management server with distributed print workers, written
in Go.

Applications (initially a web ERP) submit print jobs over HTTP. The server
holds the printer registry, the queues and the jobs. Workers located near the
printers fetch the jobs, render them for their printer and send them over the
local network. All traffic between the components runs over an embedded
[NetBird](https://netbird.io) client: platen opens no port on any host network
interface, and workers accept no inbound connections at all.

## Features

- One static, cgo-free binary for `linux/amd64` and `linux/arm64`, three
  modes: `server` (optionally with a built-in worker), `worker`, `admin`
- Embedded NetBird in userspace mode, no root required, self-hosted or cloud
  management
- Long polling leases, serial printing per printer, lease expiry with retry
  limit
- At-least-once delivery with duplicate protection: a local journal on the
  worker and idempotency keys for applications
- Backends: `raw` (e.g. ZPL), `pdf-to-raster` (PDFium in WebAssembly,
  threshold, halftone or error diffusion) and `brother-raster` (Brother
  HL-2135W: A4 at 300, 600 and 1200 dpi, Letter and A5 at 600 dpi), designed
  for more printer languages
- SQLite store (pure Go) behind an interface, documents in a spool directory

## Architecture

```
ERP ──/v1──┐                         ┌── platen worker (site A) ──TCP 9100──▶ printers
           ├── NetBird mesh ── platen server ──┤
admin ─────┘     (WireGuard)   (+ built-in     └── platen worker (site B) ──TCP 9100──▶ printers
                                 worker)
```

The server stores the original documents and stays printer-agnostic; workers
render per printer. The built-in worker runs the same worker code with an
in-process transport. Details: [docs/architecture.md](docs/architecture.md).

## NetBird setup

In short: create the groups `platen-server`, `print-workers`, `erp` and
`platen-admins`, one setup key per role (not ephemeral), and allow TCP 8080
to `platen-server` from `erp` (application API), `print-workers` (worker
API) and `platen-admins` (admin API). The setup key is read from
`PLATEN_NETBIRD_SETUP_KEY` or a file, never from the TOML configuration, and
is needed only for the first start. The policies complement platen's own
authentication; they do not replace it.

Full guide: [docs/netbird.md](docs/netbird.md).

## Quick start

```bash
# Server
platen hash-token -generate               # admin token + hash for server.toml
platen server -config /etc/platen/server.toml

# Admin (on a peer in platen-admins)
platen admin -config admin.toml worker create site1 -name "Site 1"
platen admin -config admin.toml worker enroll site1          # enrollment token
platen admin -config admin.toml printer create zebra -worker site1 -address 192.168.1.20 -backend raw
platen admin -config admin.toml client create erp -printers zebra   # app token

# Worker (at site 1, with the enrollment token in its token file)
platen worker -config /etc/platen/worker.toml

# Print from the ERP
curl -X POST "http://platen-server.netbird.cloud:8080/v1/printers/zebra/jobs?title=Label" \
     -H "Authorization: Bearer $APP_TOKEN" -H "Content-Type: application/vnd.zebra-zpl" \
     -H "Idempotency-Key: order-4711" --data-binary @label.zpl
```

## Configuration

TOML files; examples with comments in [deploy/config](deploy/config) and a
reference in [docs/operations.md](docs/operations.md). systemd units are in
[deploy/systemd](deploy/systemd).

- **Server**: NetBird (management URL, config path, setup key file, hostname,
  port), database and spool paths, retention, size limit, lease duration,
  attempts, admin token hash, `[local_worker]` (`enabled`, `name`).
- **Worker**: NetBird, server URL (NetBird DNS name), credential and journal
  paths, enrollment token file for the first start. Printers come from the
  server.
- **Admin CLI**: NetBird, server URL, admin token file.

## API overview

The full description is [api/openapi.yaml](api/openapi.yaml). JSON for
metadata, documents as raw request bodies, errors as
`{"error": {"code": "...", "message": "..."}}`.

| Family | Auth | Endpoints |
| --- | --- | --- |
| Health | none | `GET /v1/health` |
| Application | app token | `GET /v1/printers`, `POST /v1/printers/{id}/jobs`, `GET /v1/jobs/{id}`, `DELETE /v1/jobs/{id}` |
| Worker | worker credential + NetBird peer | `POST /worker/v1/register`, `GET /worker/v1/config`, `POST /worker/v1/lease`, `GET /worker/v1/jobs/{id}/document`, `POST /worker/v1/jobs/{id}/progress`, `…/complete`, `…/fail`, `POST /worker/v1/heartbeat` |
| Admin | admin token | workers, enrollment tokens, printers, app clients, jobs under `/admin/v1` |

## Development

```bash
go vet ./...
go test -race ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
```

Integration tests run server and worker without NetBird through a loopback
`mesh.Network` that exists only in test code.

## License

platen is licensed under the [Apache License 2.0](LICENSE). Dependencies and
their licenses are listed in [THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES); the
license policy and its CI checks are described in
[docs/licensing.md](docs/licensing.md). NetBird's client is used under its
BSD-3-Clause license; its AGPL-licensed parts are not used.
