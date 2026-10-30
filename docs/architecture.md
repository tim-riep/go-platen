# Architecture

platen is one binary with three modes:

```
platen server   management server, optionally with a built-in worker
platen worker   standalone worker at a remote site
platen admin    command line client for the admin API
```

```
   ERP (NetBird peer)                       admin (NetBird peer)
        │  /v1 (app token)                       │ /admin/v1 (admin token)
        ▼                                        ▼
 ┌──────────────────────── NetBird mesh (WireGuard) ───────────────────────┐
 │                                                                         │
 │   platen server ── listens only on the mesh listener (ListenTCP)        │
 │   ├─ store (SQLite) + spool (documents)                                 │
 │   ├─ lease logic, long polling, expiry, cleanup                         │
 │   └─ built-in worker "local" ── in-process transport                    │
 │            ▲                                                            │
 │            │ /worker/v1 (worker credential + peer binding)              │
 │   platen worker (site A) ── HTTP over the mesh, no inbound connections  │
 └────────────┼────────────────────────────────────────────────────────────┘
              │ raw TCP 9100 over the local network
              ▼
          printers
```

## Components

| Package | Responsibility |
| --- | --- |
| `cmd/platen` | Entry point and subcommands |
| `internal/app` | Wires server and worker mode together |
| `internal/mesh` | Embedded NetBird client, the only `mesh.Network` in production |
| `internal/server` | HTTP handlers, authentication, lease logic, cleanup |
| `internal/store` | `Store` interface, SQLite implementation, migrations |
| `internal/spool` | Document files |
| `internal/worker` | Worker loop, journal, `Transport` (HTTP and in-process) |
| `internal/backend` | `Backend` interface and registry |
| `internal/backend/raw` | Pass-through backend |
| `internal/backend/raster` | PDF → bitmap → page encoder backends (`pdf-to-raster`) |
| `internal/render` | PDF rendering with PDFium (WebAssembly), dithering |
| `internal/printer` | Raw TCP connection to the printer |
| `internal/admin` | `platen admin` commands |
| `internal/api` | JSON wire types shared by all of the above |

## Network

All traffic between platen components runs over an embedded NetBird client
in userspace mode (no root, no TUN device):

- The server listens only on `client.ListenTCP` in the mesh. It opens no TCP
  port on any host interface, not even on localhost.
- Workers and the admin CLI use `client.NewHTTPClient()` to reach the server
  by its NetBird DNS name. They set `BlockInbound`, so they accept no
  connections from the mesh, and open no local port.
- Workers reach printers through the regular local network (`net.Dial`, TCP
  port 9100), not through NetBird.

NetBird itself uses UDP sockets on the host for the WireGuard tunnel and for
connectivity checks; these belong to the mesh transport, not to platen's
APIs.

The rest of the code depends only on the small `mesh.Network` interface.
Tests use a loopback implementation that exists only in test code; the
binary has no mode that listens outside the mesh.

## Data model

- **Worker**: ID, name, location, credential hash, bound NetBird peer
  identity (WireGuard public key and FQDN), last heartbeat, version. The
  built-in worker has the fixed ID `local` and no credential.
- **Printer**: ID, name, assigned worker, address in the worker's local
  network, backend, paper size, resolution, backend options, administrative
  status (`ready`, `paused`), last reported reachability.
- **Queue**: currently one per printer, with the printer's ID. Queues and
  printers are linked by a separate table, so pools (one queue, several
  printers) can be added without changing jobs or the API.
- **Job**: ID, queue, printer, status (`queued`, `leased`, `printing`,
  `done`, `failed`, `canceled`), copies, title, content type, size,
  SHA-256 of the document, idempotency key (unique per app client), lease
  holder, lease expiry, attempts, maximum attempts, error text, timestamps.
- **AppClient**: ID, name, token hash, allowed printers (or all printers).
- **EnrollmentToken**: single use, with expiry, for one worker.

Printers are defined on the server. A worker knows only its own identity and
receives its printers from the server.

Documents are stored as files in the spool directory under the job ID; the
database holds only metadata. The server stores the original document and
stays printer-agnostic; rendering happens on the worker.

## Job life cycle

```
           submit                 lease                 progress
 (app) ──────────────▶ queued ──────────────▶ leased ──────────────▶ printing
                         ▲  │ cancel             │                       │
       retryable failure │  ▼                    │ lease expired         │ complete
       or lease expired  │ canceled              ▼                       ▼
       (attempts left)   └──────────────────── queued                   done
                                    no attempts left / permanent failure ─▶ failed
```

- **Leasing** happens in a single database transaction: the oldest queued
  job of an idle, ready printer of the requesting worker is assigned to that
  worker. A printer never has more than one leased or printing job, so
  printing is serial per printer.
- **Long polling**: `POST /worker/v1/lease` waits up to about 30 seconds and
  is woken up when jobs are created, finished or requeued.
- **Leases expire** after the lease duration (default 60 s) unless the worker
  reports progress, which it does every third of the lease duration. Expired
  jobs go back to the queue until the maximum number of attempts is reached.
- **Late reports** from the last lease holder are accepted even after the
  lease expired, as long as the job is not final. This avoids printing a job
  twice when only the report was delayed.

## Delivery semantics

Delivery is at least once, with protection against duplicate prints:

1. The worker keeps a local journal (one small JSON file per job, written
   with fsync and atomic rename).
2. Before the first byte goes to the printer, the job is journaled as
   `sent`.
3. After sending, the outcome (`printed` or `failed`) is journaled and then
   reported. The entry is removed when the server acknowledged it.
4. After a crash, `sent` entries become `uncertain`. They are reported as
   uncertain failures instead of being printed again; the server sets them to
   `failed` with a note.
5. If a job the worker already handled is leased again (because the report
   was late), the worker re-sends its report instead of printing.
6. When the server is unreachable, the worker retries with exponential
   backoff. A print in progress is completed and its report sent later.
7. Applications avoid duplicate jobs with the `Idempotency-Key` header.

A transmission error after part of the data reached the printer is reported
as uncertain as well, because the printer may have printed part of the job.
A connection error before any data was sent is retryable.

## Backends

```go
type Backend interface {
    Accepts(contentType string) bool
    Print(ctx context.Context, doc io.Reader, opts JobOptions, out io.Writer) error
}
```

Backends write to an `io.Writer`; the printer connection is a separate
layer, so backends are tested without a printer. A backend is registered with
a `backend.Factory` (name, accepted content types, constructor) in
`app.Backends()`. The server uses the same registry to validate printer
backends and to reject documents a printer's backend cannot print.

| Backend | Input | Output |
| --- | --- | --- |
| `raw` | anything | the document unchanged, repeated per copy (e.g. ZPL) |
| `pdf-to-raster` | `application/pdf` | 1-bit bitmaps as PBM (P4) pages |

Raster backends share the PDF renderer and differ only in their
`raster.Encoder`. A printer language that prints bitmaps (e.g. a Brother
raster encoder or PCL raster) implements `Encoder` and is registered with
`raster.NewFactory`. Other languages (PostScript, PCL with text) implement
`Backend` directly.

### PDF rendering

PDFs are rendered with PDFium compiled to WebAssembly (go-pdfium on wazero),
so the binary stays cgo-free. PDFium runs without file system access.
Each page is scaled to fit the printer's paper size at its resolution
(default A4, 600 dpi), centered, and landscape pages are rotated to portrait
(`rotate=none` disables this). The grayscale render is reduced to 1 bit by a
threshold (`dither=threshold`, default, with `threshold=1..255`) or by
Floyd–Steinberg error diffusion (`dither=floyd-steinberg`). These are printer
options, set by the administrator per printer.

## Security

- Tokens and credentials are random 256-bit values; only their SHA-256
  hashes are stored, and comparisons are constant time.
- Application, worker and admin routes are strictly separated, each with its
  own credential type.
- **Peer binding**: during enrollment the server maps the mesh IP of the
  incoming connection to the NetBird peer (WireGuard public key) using the
  embedded client's peer status (`IdentityForIP`). Later worker requests from
  another peer are rejected with 403, even with a valid credential.
- Documents are limited in size (default 64 MiB).
- NetBird access policies restrict which peers can reach the server at all;
  see [netbird.md](netbird.md). They complement, not replace, the checks in
  the server.
