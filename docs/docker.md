# Container image

platen is available as a container image for `linux/amd64` and
`linux/arm64`. The image contains the static platen binary and nothing else
that platen does not need (see [Contents](#contents)). It runs as a non-root
user (UID 65532).

NetBird runs in userspace mode inside the container, so the container needs
no capabilities, no `/dev/net/tun` and **no published ports**: the server is
reachable only inside the NetBird mesh, as without a container.

## Image

Images are built by CI and published to the GitHub Container Registry for
version tags:

```
ghcr.io/tim-riep/go-platen:<version>
```

To build one locally:

```bash
docker build --build-arg VERSION=$(git describe --always) -t platen:local .
```

The default command is `server -config /etc/platen/server.toml`; pass
`worker -config …` or `admin -config …` to run the other modes.

| Path | Purpose |
| --- | --- |
| `/etc/platen/` | configuration (mount read-only) |
| `/var/lib/platen` | server state: database, spool, journal of the built-in worker, NetBird identity |
| `/var/lib/platen-worker` | worker state: credential, journal, NetBird identity |
| `/run/secrets/…` | setup key and enrollment token as Docker secrets (optional) |

Keep the state directories on persistent volumes. The NetBird identity in
them keeps the peer and its mesh IP stable, and a worker's credential is
bound to that peer.

## Server

[`deploy/docker/compose.yaml`](../deploy/docker/compose.yaml) runs the server
with the built-in worker:

```bash
cd deploy/docker
mkdir -p secrets
printf '%s' "<setup key>" > secrets/netbird-setup-key
docker run --rm ghcr.io/tim-riep/go-platen hash-token -generate   # admin token and hash
# put the hash into server.toml, adjust management_url and hostname
docker compose up -d
docker compose logs -f
```

The log shows `connected to netbird` with the server's mesh IP and DNS name,
then `server listening in the mesh`.

## Worker

[`deploy/docker/compose.worker.yaml`](../deploy/docker/compose.worker.yaml)
runs a worker at a remote site. Create the worker and an enrollment token
with the admin CLI first, then:

```bash
printf '%s' "<setup key>" > secrets/netbird-setup-key
printf '%s' "<enrollment token>" > secrets/enrollment-token
docker compose -f compose.worker.yaml up -d
```

The worker enrolls on its first start and stores its credential in the
volume. Printers are reached through the host's network with the default
bridge network; no host networking is needed.

## Admin CLI

The admin CLI joins the mesh as its own peer. Keep its NetBird identity in a
volume so that it does not register a new peer every time:

```bash
docker volume create platen-admin
docker run --rm \
  -e PLATEN_NETBIRD_SETUP_KEY="<setup key>" -e PLATEN_ADMIN_TOKEN="<admin token>" \
  -v platen-admin:/var/lib/platen -v "$PWD/admin.toml:/etc/platen/admin.toml:ro" \
  ghcr.io/tim-riep/go-platen admin -config /etc/platen/admin.toml worker list
```

with an `admin.toml` whose `netbird.config_path` lies in the volume, e.g.
`/var/lib/platen/netbird-admin/config.json`.

## Contents

The runtime image is built `FROM scratch` and contains only:

| Path | Content | License |
| --- | --- | --- |
| `/usr/local/bin/platen` | the platen binary | Apache-2.0; its dependencies in `/usr/share/doc/platen/THIRD_PARTY_LICENSES` |
| `/usr/share/doc/platen/` | `LICENSE`, `THIRD_PARTY_LICENSES` | |
| `/etc/ssl/certs/ca-certificates.crt` | Mozilla's CA certificate bundle as packaged by Debian, unmodified (TLS to the NetBird management server) | MPL-2.0; copyright file in `/usr/share/doc/ca-certificates/` |
| `/etc/os-release` | a three-line file naming platen, read by NetBird | part of platen |
| `/tmp`, `/var/lib/platen`, `/var/lib/platen-worker` | empty directories | |

The build stage uses the official `golang` image; nothing else from it ends
up in the runtime image.

## Tested

The image was tested on 2026-10-31 against a self-hosted NetBird: a server
container joined the mesh and listened only there, an admin container called
the admin API by the server's NetBird DNS name, a worker container enrolled
and was bound to its NetBird peer, and a container with a copy of that
worker's credential but its own NetBird identity was rejected with 403.
