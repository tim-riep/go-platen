# NetBird setup

platen embeds the NetBird client (`github.com/netbirdio/netbird/client/embed`,
BSD-3-Clause). Each platen process is its own NetBird peer. It works with
NetBird cloud and with a self-hosted NetBird management server
(`netbird.management_url`).

## Groups

Create these groups in NetBird:

| Group | Members |
| --- | --- |
| `platen-server` | the platen server peer |
| `print-workers` | all remote platen workers |
| `erp` | the application servers that submit print jobs |
| `platen-admins` | peers that run `platen admin` |

## Setup keys

Create one setup key per role and assign the auto-group:

| Setup key | Auto-group | Type |
| --- | --- | --- |
| platen server | `platen-server` | one-off |
| platen workers | `print-workers` | reusable, or one-off per site |
| platen admin | `platen-admins` | one-off per administrator |

Do not mark the keys as ephemeral: platen keeps a persistent peer identity
(`netbird.config_path`), and its peer must survive restarts so that its
identity and mesh IP stay stable. The worker's peer identity is bound to its
platen credential at enrollment.

The setup key is never written into the TOML configuration. platen reads it
from the environment variable `PLATEN_NETBIRD_SETUP_KEY` or from
`netbird.setup_key_file`. It is needed only for the first start; afterwards
the peer logs in with the identity stored at `netbird.config_path`, and the
key file can be deleted.

## Policies

Create access policies (TCP, port `8080` or your `netbird.port`):

| Policy | Source | Destination | Port | Purpose |
| --- | --- | --- | --- | --- |
| erp-to-platen | `erp` | `platen-server` | 8080/TCP | application API `/v1` |
| workers-to-platen | `print-workers` | `platen-server` | 8080/TCP | worker API `/worker/v1` |
| admins-to-platen | `platen-admins` | `platen-server` | 8080/TCP | admin API `/admin/v1` |

Remove the default "All to All" policy, or at least make sure it does not
include these groups. Workers and admin peers block inbound connections
anyway (`BlockInbound`); no policy towards them is needed.

NetBird policies work on ports, not on URL paths, so the separation of the
API families is enforced by platen: each family has its own credential type,
and worker credentials are additionally bound to the worker's NetBird peer.
The policies reduce the attack surface; they do not replace these checks.

## DNS

Workers and the admin CLI address the server by its NetBird DNS name, e.g.
`http://platen-server.netbird.cloud:8080` (cloud) or
`http://platen-server.<your dns domain>:8080` (self-hosted). The name
is the server's `netbird.hostname`. If DNS is not available in your setup,
the server's mesh IP works as well; it stays stable because the peer
identity is persistent.

## Troubleshooting

- `netbird.log_level = "debug"` makes NetBird verbose; its messages appear
  in platen's log with `component=netbird`.
- platen logs when the connection to NetBird's management or signal service
  is lost and when it is restored. The client reconnects on its own.
- A worker that was re-registered as a new NetBird peer (for example after
  deleting its `config_path`) is rejected by the server with 403, because
  its credential is bound to the old peer. Delete the worker's credential
  file, create a new enrollment token and enroll it again.
