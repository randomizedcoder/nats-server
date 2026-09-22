# Cluster routes over Unix domain sockets

These configs run a three-server NATS cluster whose routes use Unix domain
sockets instead of TCP. They are driven by `scripts/uds-cluster-smoke.sh`,
which builds the server, starts the three processes, checks the mesh through
`/routez` and `/varz`, captures `ss -xlp` / `ss -xp` output, pushes traffic,
crashes and restarts one server, and verifies that no socket files are left
behind on shutdown. The design is in `doc/uds-cluster-routes.md`.

## Configuration syntax

A cluster listener or route is a unix socket when its address uses the
`unix://` scheme:

```hcl
cluster {
  name: uds
  listen: "unix:///tmp/nats-uds/a.sock"        # pathname socket
  # listen: "unix://@nats-route-a"             # Linux abstract socket
  routes = [
    "unix:///tmp/nats-uds/b.sock"
    "unix:///tmp/nats-uds/c.sock"
  ]
}
```

- `unix:///absolute/path` is a filesystem socket; `unix://@name` is a Linux
  abstract socket. Relative paths, `%` escapes and empty names are rejected.
- The whole path must fit in `sun_path`: 107 bytes on Linux for pathname
  sockets. Keep socket directories short.
- `listen: "unix://..."` cannot be combined with `host`/`port`, and cannot be
  changed by config reload.
- A unix listener advertises nothing to its peers unless `advertise` is set to
  a `unix://` address, so every edge of the mesh should be an explicit route.
- Route credentials cannot be placed inside a `unix://` URL (`@` marks an
  abstract socket). Explicit unix routes authenticate with the dialing server's
  own cluster `authorization { user, password }` block.
- The same `unix://` form works for `-cluster`, `-routes` and
  `-cluster_advertise` on the command line.

Monitoring: `/varz` adds `cluster.unix_socket` and `cluster.unix_socket_stats`
(`accepted`, `dialed`, `dial_errors`, `stale_removed`, `active`); `/routez`
adds `transport` (`tcp` or `unix`) and, for solicited unix routes,
`unix_socket`. TCP-only servers see no new fields.

## Files

| File | Topology |
|---|---|
| `srv_a.conf`, `srv_b.conf`, `srv_c.conf` | Full mesh: each server listens on its own socket and dials the other two. |
| `srv_a_pairs.conf`, `srv_b_pairs.conf`, `srv_c_pairs.conf` | Ring through a proxy: each server dials one proxy socket (`ab`, `bc`, `ca`) which forwards to the next server. Used with `UDS_PAIRS=1`. |

All files use `/tmp/nats-uds`; the script rewrites copies when `UDS_DIR` is set.

## Running

```
scripts/uds-cluster-smoke.sh                       # full mesh
UDS_PAIRS=1 scripts/uds-cluster-smoke.sh           # ring, socat stands in for the proxy
UDS_PAIRS=1 UDS_PROXY=1 scripts/uds-cluster-smoke.sh   # ring, external proxy serves ab/bc/ca.sock
```

Needs `go`, `curl`, `ss` and the `nats` CLI (run through
`nix shell nixpkgs#natscli` when absent); `socat` for `UDS_PAIRS=1`. Logs, raw
monitoring bodies and `ss` output are written to `$OUT` (a `mktemp` directory
by default) for pasting into a PR.
