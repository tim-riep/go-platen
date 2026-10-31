# Web panel

The server serves a web panel on its port in the mesh (default 80), e.g.
`http://platen-server.<your NetBird DNS domain>/`. It offers everything the
admin API offers (workers and enrollment tokens, printers and their options,
app clients and their tokens, jobs, printing a file) plus the management of
panel users.

**Jobs**: admins can cancel queued jobs, **retry** failed or canceled jobs
(they go back into the queue with the same document and fresh attempts; a
job that failed as *uncertain* may already have been printed) and **delete**
jobs with their documents, except while a worker processes them. The CLI
offers the same with `platen admin job cancel|retry|delete <id>`.

**Printing a file**: admins can print a file on any printer with *Print…* in
the printer list or *Print a file* under Jobs. The file is sent as it is, as
the ERP does: PDF for `pdf-to-raster` and `brother-raster` printers, the
printer's own language (e.g. ZPL) for `raw` printers. The job records
`panel:<NetBird IP>` as its submitter; the admin API and
`platen admin job print <printer> <file>` record `admin`. Like
the APIs, it is reachable only inside the NetBird mesh. It is part of the
binary and loads nothing from the internet.

## Users and roles

Panel users are identified by their **NetBird IPv4 address**. Access also
requires the **NetBird peer** (its WireGuard public key) that used the
address when it was granted, so a device that later receives the same
address (for example after a peer was deleted and its IP reused) does not
inherit the access.

- **The first peer that opens the panel becomes its admin.** Open the panel
  from your own device right after the first start of the server. The server
  logs `panel: first admin claimed` with the IP and peer.
- Admins add further users under *Panel users* by NetBird IPv4 address with
  the role **admin** (everything) or **viewer** (read only). The peer behind
  the address is bound on the user's first visit.
- *Reset peer* clears a user's binding, e.g. after the device was
  re-registered in NetBird; the next visit binds the new peer.
- The last admin can be neither removed nor demoted.

Panel users need no token; the NetBird identity is the credential. The NetBird
access policies decide which peers can reach the server at all
([netbird.md](netbird.md)).

## Recovery

If no admin can open the panel any more, use the admin API with the admin
token:

```bash
platen admin -config admin.toml panel-user list
platen admin -config admin.toml panel-user add 100.64.0.12 -name "Laptop" -role admin
platen admin -config admin.toml panel-user reset 100.64.0.12
```

## Protection against requests from other web pages

Because access depends on where a request comes from, a malicious web page
opened in a panel user's browser must not be able to use the panel in their
name:

- The panel answers only requests whose `Host` is the server's NetBird name,
  its short name, its mesh IP or a name in `panel.allowed_hosts`. This blocks
  DNS rebinding.
- Every panel API request must carry the header `X-Platen-Panel`. Browsers
  send custom headers to another origin only after a CORS preflight, which
  the server never grants.
- State-changing requests with an `Origin` header must come from the server
  itself.
- The page is served with a strict Content Security Policy.

## Configuration

```toml
[panel]
enabled = true                 # default
allowed_hosts = ["print.example.internal"]   # optional extra host names
```
