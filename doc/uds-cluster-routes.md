# Design: Cluster routes over Unix domain sockets (UDS)

Status: DRAFT for discussion (open a GitHub issue before the PR, per `CONTRIBUTING.md`).
Progress is tracked in [uds-cluster-routes-status.md](uds-cluster-routes-status.md).
Author: Dave Seddon
Date: 2026-09-22

## 1. Objective

Allow `nats-server` cluster **routes** (the server-to-server full mesh) to be carried
over Unix domain sockets instead of TCP, so that a cluster can be run on top of a
UDS-over-RDMA tunnel (the "half kernel bypass" `uds-over-rdma-proxy`) for lower latency
and higher throughput than the host TCP stack.

In scope:

- A route **listener** on a UDS (pathname socket everywhere, Linux abstract socket
  optionally).
- Route **solicitation** (dialing) to a UDS.
- Mixed clusters: a server may listen on UDS and dial some peers over TCP and others
  over UDS, and the reverse.
- Config file, CLI flags, `nats-server -t` (config check), config reload semantics.
- Monitoring (`/varz`, `/routez`, `$SYS ... STATSZ`) fields for UDS transports so the
  out-of-tree Prometheus exporter can surface them.
- Cross-platform behaviour: Linux, macOS, the BSDs, Windows (AF_UNIX since Windows 10
  1803), and a compile-only fallback for `wasm`.
- Comprehensive table-driven unit tests plus in-process and multi-process integration
  tests (three servers in a full mesh over UDS on one machine).

Out of scope (called out in §11 so they can be follow-ups):

- Client, leafnode, gateway, MQTT and websocket listeners over UDS.
- More than one route listener per server (the repo `TODO.md` already tracks
  "Multiple listen endpoints").
- Socket file permission management beyond what the directory provides.
- Any new Go module dependency. `CONTRIBUTING.md` forbids non-essential dependencies,
  and nothing here needs one.

## 2. Background: how routes work today

Everything below is TCP-specific and has to be made transport-aware. File and line
references are against `main` at commit `edb1b17a`.

| Concern | Where | What it assumes |
|---|---|---|
| Listener | `server/route.go:2728` `startRouteAcceptLoop` | `natsListen("tcp", host:port)`, then `l.Addr().(*net.TCPAddr).Port` (panics on `*net.UnixAddr`) at `:2761` and `:2801`. |
| Advertised address | `server/route.go:2862` `setRouteInfoHostPortAndIP` | Fills `routeInfo.Host`, `.Port`, `.IP` (`nats-route://host:port/`) from `Cluster.Advertise` or listen host/port. |
| Self-route detection | `server/route.go:2835` | `s.routesToSelf` keyed by `ip:port` for every local interface. |
| Dialing | `server/route.go:2923` `connectToRoute` | `s.getRandomIP(resolver, rURL.Host, ...)` then `natsDialTimeout("tcp", ...)`. |
| Gossip / implicit routes | `server/route.go:847` `processRouteInfo`, `:1052` `processImplicitRoute`, `:1113` `hasThisRouteConfigured` | Builds `nats-route://Host:Port/` when a URL is unknown; derives `info.IP` from `conn.RemoteAddr().(*net.TCPAddr)` for `*net.TCPConn` **and `*tls.Conn`** (a TLS route over UDS would panic there; a plain `*net.UnixConn` falls to the `default` branch); compares configured routes by `host:port`. |
| "Is clustering enabled" | `server.go:815,881,1135,1578,1952,2556,4008,4413`, `events.go:914`, `opts.go:6042,6477` | `opts.Cluster.Port != 0`. |
| Public accessor | `server/server.go:3975` `ClusterAddr() *net.TCPAddr` | Type assertion on the listener address. |
| Ports file / `PortsInfo` | `server/server.go:4218` `resolveHostPorts`, `:4247` `formatURL` | Type assertion `addr.Addr().(*net.TCPAddr)`. |
| Monitoring | `server/monitor.go:917` (Routez), `:1322` `ClusterOptsVarz`, `:1762` `urlsToStrings(opts.Routes)` | `RouteInfo.IP/Port` from `*net.TCPAddr` (same `*tls.Conn` panic shape as above); `ClusterOptsVarz.Host/Port`. `urlsToStrings` uses `url.String()` and is fine for UDS URLs; `getURLsAsString` (`server/util.go:329`) returns `u.Host` and would drop a path, but it is only used for gateways. |
| Config parsing | `server/opts.go:1998` `parseListen`, `:2047` cluster `listen`, `:2106` `routes` via `parseURLs`/`parseURL`, `:5988` `RoutesFromStr`, `:6519` `overrideCluster` (`-cluster` flag) | `net.SplitHostPort` everywhere; route URLs are `nats-route://[user:pass@]host:port`. |
| Reload | `server/reload.go:1436-1470`, `:1698`, `:2804` `validateClusterOpts` | `Host`/`Port` are explicitly non-reloadable; `Advertise` is validated with `parseHostPort`, which rejects a `unix://` value; port `-1` is restored from the running value. |
| TLS name | `server/client.go:6708` `doTLSHandshake`, `server/route.go:3022` `saveRouteTLSName` | `ServerName` comes from `url.Hostname()`, falling back to the first hostname among configured routes. |
| Client identity | `server/client.go:785` | `net.SplitHostPort(RemoteAddr().String())`; errors are ignored so a UDS peer yields empty host/port rather than a crash. |

The shared accept loop (`server/server.go:2899` `acceptConnections`) and everything above
the `net.Conn` (auth, INFO exchange, compression, pooling, per-account routes) are already
transport-agnostic. The Go standard library gives us `net.Listen("unix", path)` and
`net.Dialer.Dial("unix", path)` on every target platform, so no platform code beyond path
normalisation is needed.

## 3. User-facing design

### 3.1 Address syntax

Follow the form used by containerd PR 13569 (`dial_addr`), which is the same shape as
Docker, gRPC and systemd use:

| Form | Meaning | Platforms |
|---|---|---|
| `unix:///abs/path/route.sock` | Pathname socket. Note the three slashes. | All |
| `unix://@name` | Abstract socket (leading `@` becomes a NUL byte). Accepted by the parser everywhere; only Linux services it, elsewhere `net.Listen`/`Dial` fails at runtime. | Linux |
| `unix:///C:/ProgramData/nats/route.sock` | Windows pathname socket in `file://`-style URL form (forward slashes, leading `/` before the drive letter). Converted to `C:\ProgramData\nats\route.sock` before the syscall. | Windows |

Validation rules (all produce a config error with the offending value quoted):

1. No leading or trailing whitespace.
2. Scheme `unix://` is required and case-insensitive.
3. The remainder must be non-empty, contain no `?`, `#` or `%`, and must `url.Parse`.
   `%` is rejected because the canonical `url.URL` form (§4.2) would re-encode it on the
   wire and the gossiped address would no longer match the configured one.
4. Pathname form: absolute (`/...`), no trailing `/`.
5. Abstract form: at least one character after `@`.
6. The **native** form must fit `sun_path`: 108 bytes on Linux, Solaris and Windows,
   104 on macOS and the BSDs. The limit is taken from
   `len(syscall.RawSockaddrUnix{}.Path)` so it always matches the build target. A
   pathname may use at most `sun_path - 1` bytes (Go's `SockaddrUnix` reserves the
   terminating NUL on every OS and returns `EINVAL` otherwise); an abstract name may
   fill `sun_path` because its `@` stands in for the leading NUL.

Credentials for route authentication cannot be carried in a `unix://` route URL:
`unix://ruser:top_secret@/run/nats/b.sock` is **not** valid (the `@` would collide with
the abstract-socket marker). Instead the dialing server's own cluster `authorization {}`
block is used. Gossiped implicit routes already work this way (`processImplicitRoute`
injects `opts.Cluster.Username/Password` into the URL), and `sendRouteConnect` applies the
same fallback for explicit `unix://` routes whose URL has no user-info. TCP routes are
unchanged: a `nats-route://` URL without user-info still sends no credentials. This is a
deliberate simplification and is documented.

### 3.2 Configuration

```hcl
cluster {
  name: A
  # Listen on a Unix socket instead of host:port. Mutually exclusive with
  # host/port/net. A stale socket file left by a crashed server is removed
  # automatically when nothing is listening on it.
  listen: "unix:///run/nats/a.sock"

  # Optional. What peers should be told to dial to reach this server. When the
  # listener is a UDS and advertise is unset, nothing is advertised: peers learn
  # about this server through gossip but do not attempt to dial it. See §4.4.
  advertise: "unix:///run/uds-proxy/a.sock"

  routes: [
    "unix:///run/uds-proxy/ab.sock",   # UDS route
    "nats-route://10.0.0.3:6222",       # TCP route in the same cluster is fine
  ]
}
```

CLI equivalents: `-cluster unix:///run/nats/a.sock`,
`-routes unix:///run/nats/b.sock,unix:///run/nats/c.sock`,
`-cluster_advertise unix:///run/uds-proxy/a.sock`.

New `ClusterOpts` field:

```go
// UnixSocket, when non-empty, makes the route listener bind a Unix domain
// socket instead of Host:Port. It holds the URL-form address after "unix://"
// ("/run/nats/a.sock", "@nats-a", or "/C:/nats/a.sock" on Windows).
UnixSocket string `json:"-"`
```

The `json:"-"` tag follows the note on `ClusterOpts` that its JSON tags are deprecated;
monitoring has its own struct (§3.3). `setBaselineOptions` (`server/opts.go:6042`) must
only default `Host` to `DEFAULT_HOST` when `UnixSocket` is empty, otherwise every UDS
configuration would fail the mutual-exclusion rule below and every reload would see a
`Host` change.

`Host`/`Port` remain zero when `UnixSocket` is set. `Advertise` keeps its type and may
now hold a `unix://` URL. `Options.Routes` stays `[]*url.URL`; UDS entries are stored in
a **canonical** form `&url.URL{Scheme: "unix", Path: "<addr>"}` so that the existing
`reflect.DeepEqual`-based `urlsAreEqual` and `routeStillValid` keep working.

Validation (in `validateCluster`, `server/server.go:1107`, plus parse-time checks):

| Condition | Result |
|---|---|
| `UnixSocket` set and `Host != ""` or `Port != 0` | error: `cluster: unix socket listen and host/port are mutually exclusive` |
| `Advertise` is `unix://...` but listener is TCP | error: `cluster: advertise transport "unix" does not match listener transport "tcp"` |
| `Advertise` is `host:port` but listener is UDS | error, mirrored |
| abstract socket on a non-Linux build | warning at parse time, runtime error from `net.Listen` |
| TLS configured with a UDS listener | allowed (see §4.6) |

### 3.3 Monitoring and metrics

There is no Prometheus or OpenTelemetry dependency in the server and `CONTRIBUTING.md`
discourages adding one. The repository convention is: atomic counters on `Server`,
surfaced as JSON on `/varz`, `/routez` and `$SYS.SERVER.<id>.STATSZ`, and scraped by the
out-of-tree `prometheus-nats-exporter`. This design follows that convention; the
exporter needs a companion change to map the new fields, tracked as a follow-up in §11.

New fields (all `omitempty`, each with the inline `//` doc comment `monitor.go` uses):

| Struct | Field | JSON | Meaning |
|---|---|---|---|
| `ClusterOptsVarz` | `UnixSocket string` | `unix_socket` | Listener socket (URL form) when the route listener is a UDS. |
| `ClusterOptsVarz` | `UnixSocketStats *RouteUnixSocketStats` | `unix_socket_stats` | Counters below. Nil unless the listener is a unix socket or any counter is non-zero, so a TCP-only server's `/varz` is byte-identical to today. |
| `RouteUnixSocketStats` | `Accepted uint64` | `accepted` | Route connections accepted on the UDS listener. |
| `RouteUnixSocketStats` | `Dialed uint64` | `dialed` | Successful outbound UDS route dials. |
| `RouteUnixSocketStats` | `DialErrors uint64` | `dial_errors` | Failed outbound UDS route dials (per attempt). |
| `RouteUnixSocketStats` | `StaleRemoved uint64` | `stale_removed` | Stale socket files removed at listener start. |
| `RouteUnixSocketStats` | `Active int` | `active` | Currently connected routes whose transport is UDS. |
| `RouteInfo` (routez) | `Transport string` | `transport` | `"tcp"` or `"unix"`. Always set. |
| `RouteInfo` (routez) | `UnixSocket string` | `unix_socket` | Peer socket path for solicited UDS routes; empty for accepted UDS routes because the peer end is unnamed. `IP`/`Port` are left zero. |
| `RouteStat` (STATSZ) | `Transport string` | `transport` | Same as above (`omitempty`, only empty for a route with no connection) so system-account consumers (e.g. `nats-surveyor`) can split traffic by transport. |
| `Ports.Cluster` (ports file) | existing `[]string` | `cluster` | Gains entries of the form `unix:///run/nats/a.sock`. |

Counters live on a new `udsStats` struct embedded in `Server` next to `scStats` and
`staleStats` (`server/server.go:169`), using `atomic.Uint64`, and are copied into `Varz`
in `updateVarzRuntimeFields` (`server/monitor.go:1907`). `Active` is computed under the
server lock by `forEachRoute`.

### 3.4 Logging

- `Listening for route connections on unix socket /run/nats/a.sock` (Noticef).
- `Removed stale route unix socket /run/nats/a.sock` (Warnf).
- `Trying to connect to route on unix socket /run/uds-proxy/ab.sock` (Debugf).
- Existing `Error trying to connect to route (attempt N): ...` unchanged; the error from
  `net.Dial` already names the path.

## 4. Internal design

### 4.1 New file layout

| File | Build tag | Contents |
|---|---|---|
| `server/uds.go` | none | `parseUnixAddr`, `unixRouteURL`, `unixAddrFromRouteURL`, `isUnixRouteURL`, `listenRouteUnix`, `routeTransport`, stale-socket handling, `udsStats`. |
| `server/uds_unix.go` | `//go:build !windows` | `nativeUnixAddr` (identity), `isConnRefused` via `errors.Is(err, syscall.ECONNREFUSED)`. |
| `server/uds_windows.go` | `//go:build windows` | `nativeUnixAddr` (strip `/` before drive letter, `filepath.FromSlash`), `isConnRefused` via `windows.WSAECONNREFUSED` from `golang.org/x/sys` (already a dependency). |
| `server/uds_sockaddr.go` | `//go:build !wasm` | `const maxUnixSocketPathLen = len(syscall.RawSockaddrUnix{}.Path)`. |
| `server/uds_sockaddr_wasm.go` | `//go:build wasm` | `const maxUnixSocketPathLen = 108` so the package still compiles for the existing wasm targets (`disk_avail_wasm.go` precedent). |
| `server/uds_test.go`, `server/uds_windows_test.go`, `server/routes_uds_test.go` | as above | Tests, §6. |

Copyright headers and `//go:build` placement follow `server/disk_avail_windows.go`
(header, blank line, tag at line 14, blank line, `package server`).

### 4.2 Parsing and canonical form

```go
// parseUnixAddr validates a "unix://" address (see §3.1) and returns the
// URL-form address after the scheme, e.g. "/run/nats/a.sock" or "@nats-a".
func parseUnixAddr(raw string) (string, error)

// unixRouteURL builds the canonical *url.URL used in Options.Routes and in
// route INFO.IP for a validated address.
func unixRouteURL(addr string) *url.URL { return &url.URL{Scheme: "unix", Path: addr} }

// unixAddrFromRouteURL accepts both the canonical form and a freshly
// url.Parse'd "unix://..." and returns the address, or ok=false when u is
// not a unix route URL.
func unixAddrFromRouteURL(u *url.URL) (addr string, ok bool)
```

`url.Parse` behaviour that the helper has to absorb (verified with Go 1.26):

| Input | `User` | `Host` | `Path` | Recovered address |
|---|---|---|---|---|
| `unix:///run/a.sock` | nil | `` | `/run/a.sock` | `/run/a.sock` |
| `unix://@name` | non-nil, empty username | `name` | `` | `@name` |
| `unix://@nats/a` | non-nil, empty username | `nats` | `/a` | `@nats/a` |
| canonical `{Scheme:"unix", Path:"@name"}` | nil | `` | `@name` | `@name`; `String()` gives `unix://@name`, which re-parses to the row above |

Rule: if `User != nil` with an empty username and no password, the address is
`"@" + Host + Path`; otherwise it is `Path`. `Hostname()` is never used for UDS URLs, and
`Redacted()` (used in the existing log lines) prints the canonical form unchanged.

Hook points:

- `parseURL` (`server/opts.go:2266`): when `typ == "route"` and the string starts with
  `unix://` (case-insensitive), run `parseUnixAddr` and return `unixRouteURL(addr)`.
- `RoutesFromStr` (`server/opts.go:5988`): same. Today it swallows `url.Parse` errors;
  UDS validation errors must not be swallowed, so `RoutesFromStr` gains an error-returning
  sibling used by flag processing while the exported signature is preserved.
- `parseCluster` `listen` (`server/opts.go:2047`) and `overrideCluster`
  (`server/opts.go:6519`): if the string starts with `unix://`, set
  `opts.Cluster.UnixSocket` and leave `Host`/`Port` zero. `parseListen` itself is shared
  with other listeners and is left untouched; the cluster case branches before calling it.
- `cluster_advertise` / `advertise`: stored verbatim; validated in `validateCluster`.

### 4.3 Listener

`startRouteAcceptLoop` becomes:

```go
var l net.Listener
if us := opts.Cluster.UnixSocket; us != _EMPTY_ {
    l, e = s.listenRouteUnix(us)              // §4.3.1
} else {
    l, e = natsListen("tcp", hp)              // unchanged
}
```

with the two `(*net.TCPAddr)` assertions replaced by a `switch a := l.Addr().(type)` that
only writes back the ephemeral port for `*net.TCPAddr`. The Notice line reports the
socket path for `*net.UnixAddr`.

#### 4.3.1 Stale socket handling (pathname sockets only)

A pathname socket outlives a crashed process. `net.Listen("unix", p)` then fails with
`EADDRINUSE`. The idiomatic and safe sequence:

1. `os.Lstat(native)`. `ENOENT` → listen.
2. Not a socket (`mode&os.ModeSocket == 0`) → error `route unix socket path %q exists and is not a socket`. Never remove.
3. Probe: `net.DialTimeout("unix", native, 250ms)`. Success → close and return error
   `route unix socket %q is in use by another process`.
4. Probe returned `ECONNREFUSED` → `os.Remove(native)`, log Warnf, bump `StaleRemoved`, listen.
5. Any other probe error → return it unchanged.

Go's `*net.UnixListener` unlinks the file on `Close()` (`SetUnlinkOnClose` defaults to
true for listeners it created), so `Shutdown` (`server/server.go:2688`) needs no change
beyond what it does today. Windows removes the file on close as well; CI on the existing
`windows-2022`/`windows-2025` matrix verifies this.

Directory permissions are the access-control mechanism, exactly as noted in the
containerd PR: the server must be able to `bind()` in the directory, and only the peers
(or the proxy) that should connect should be able to reach the socket. Document this in
the config reference; do not add a mode option in v1.

#### 4.3.2 Self-route detection

Besides the `ip:port` entries in `s.routesToSelf`, record the native listen path and the
advertised UDS path (if any) in a new `s.unixRoutesToSelf map[string]struct{}`. The
UDS dial path (§4.4) checks it before dialing and returns immediately, mirroring the
`errNoIPAvail` early return for TCP.

### 4.4 Dialing and gossip

`connectToRoute` (`server/route.go:2923`) gets a transport branch:

```go
if addr, ok := unixAddrFromRouteURL(rURL); ok {
    if s.isUnixRouteToSelf(addr) { return }
    conn, err = natsDialTimeout("unix", nativeUnixAddr(addr), DEFAULT_ROUTE_DIAL)
} else {
    address, err := s.getRandomIP(...)                      // unchanged
    ...
}
```

`natsDialTimeout` already takes a network argument; `KeepAlive: -1` is a no-op for
`unix` and harmless.

Gossip semantics with a UDS listener:

| `cluster.advertise` | `routeInfo` sent to peers | Peer behaviour |
|---|---|---|
| unset | `Host=""`, `Port=0`, `IP=""` | Peer records the server (name/ID, dedup, per-account routing) but **does not** attempt an implicit dial: `processImplicitRoute` returns early when there is no dialable address, and `processRouteInfo` skips manufacturing a `nats-route://host:port/` URL only when both `Host` and `Port` are zero (TCP accept-side behaviour is unchanged). |
| `unix://...` | `Host=""`, `Port=0`, `IP="unix:///path"` | Peer runs `hasThisRouteConfigured` comparing UDS addresses; if not configured it dials the path with the existing Implicit retry policy (`connect_retries`). |

Rationale: a socket path is meaningful only inside one host's filesystem namespace (or
one proxy's namespace). Advertising the listen path by default would make every remote
server log dial errors for a path it cannot reach. The full mesh is therefore formed from
explicit routes, which is also what the three-process test in §7 relies on.

`processRouteInfo` (`server/route.go:847`): the switch on the connection type is replaced
by a switch on `c.nc.RemoteAddr().(type)`, so a `*tls.Conn` wrapping a `*net.UnixConn` is
handled by the address and not by the wrapper. `*net.TCPAddr` keeps today's behaviour;
`*net.UnixAddr` sets `info.IP = c.route.url.String()` only when a URL is known (solicited
side) and otherwise leaves `info.IP` empty. `Routez` gets the identical switch.

Backward compatibility: a pre-UDS server that receives `Host=""`/`Port=0` would build
`nats-route://:0/` and fail to dial it. Mixed old/new clusters that include UDS servers
are therefore unsupported and the release note says so. TCP-only clusters see no wire
change.

### 4.5 "Clustering enabled" predicate

Introduce

```go
// listenEnabled reports whether a route listener is configured.
func (c *ClusterOpts) listenEnabled() bool { return c.Port != 0 || c.UnixSocket != _EMPTY_ }
```

and use it at the twelve sites that gate on `Cluster.Port`: the ones listed in §2 plus
`server/jetstream.go:2974` (clustered JetStream requires `server_name`/`cluster.name`),
`server/mqtt.go:721` and `server/mqtt.go:750`. Two of them deserve a note:
`server/server.go:2556` is what starts `StartRouting` at all, and `server/opts.go:6477` is
the `solicited routes require cluster capabilities, e.g. --cluster` guard, which already
accepts `-cluster unix://...` (the flag sets `ListenStr`) but must also accept a config-file
`listen: "unix://..."` combined with a `-routes` flag. A test greps non-test `server/*.go`
for `Cluster.Port` comparisons so a new site cannot slip in. `reload.go`'s `clusterOrgPort`
restore needs no change: it exists only because an ephemeral `-1` port is rewritten by the
accept loop, and a socket path has no ephemeral component. `validateClusterOpts`
(`server/reload.go:2804`) adds `UnixSocket` to the non-reloadable list next to `Host` and
`Port`, and validates `Advertise` with `parseUnixAddr` when it starts with `unix://`
instead of `parseHostPort`.

### 4.6 TLS over UDS

The handshake code is transport-agnostic. The only gap is `ServerName`:
`doTLSHandshake` (`server/client.go:6708`) uses `url.Hostname()`, which is empty for a
UDS URL. Change the fallback to: hostname, else `tlsName` (already the fallback for IP
literals), else, when the URL is a UDS URL and `InsecureSkipVerify` is false, return a
clear error
`TLS route over unix socket requires cluster.tls.insecure or a configured TCP route hostname`.
With `insecure: true` Go accepts an empty `ServerName`, so the error is conditional.
Operators running UDS behind an RDMA tunnel are expected to run without route TLS; the
option remains available for mixed clusters.

### 4.7 Public API and monitoring code paths

- `ClusterAddr() *net.TCPAddr` keeps its signature and returns `nil` for a UDS listener
  (documented). Add `ClusterUnixAddr() *net.UnixAddr` and `ClusterListenAddr() net.Addr`.
- `resolveHostPorts`/`formatURL` (`server/server.go:4218`): type switch; `*net.UnixAddr`
  yields `unix://<name>` and ignores the `nats`/`tls` protocol argument that `PortsInfo`
  picks at `server/server.go:4299`, since it is meaningless for a socket.
- `Routez` (`server/monitor.go:917`): switch on `RemoteAddr().(type)` and set `Transport`.
- `client.go:785`: for a `*net.UnixAddr`, set `c.host` to the socket name and `c.port` to
  0 instead of relying on the ignored `SplitHostPort` error. Only the soliciting side gets
  a meaningful name; the accepted end of a UDS is unnamed (`""` or `"@"` on Linux).
- `healthz` (`server/server.go:4008`) and `serviceListeners` (`:4413`) use
  `listenEnabled()`.

## 5. Implementation plan

Each phase is independently reviewable and leaves `main` green. Commits are signed off
(`git commit -s`) and squashed per phase before the PR is marked ready.

| # | Phase | Files | Exit criteria |
|---|---|---|---|
| 0 | Open a GitHub issue describing the feature and linking this doc. | — | Maintainer acknowledgement. |
| 1 | Address parsing, canonical URL helpers, per-platform `nativeUnixAddr` and `maxUnixSocketPathLen`. | `server/uds.go`, `uds_unix.go`, `uds_windows.go`, `uds_sockaddr*.go` | §6.1–6.3 tables pass on Linux and under `GOOS=windows go vet`. |
| 2 | Options: `ClusterOpts.UnixSocket`, `listen`/`-cluster`/`routes`/`-routes`/`advertise` parsing, `listenEnabled()`, `validateCluster` rules, reload rejection. | `server/opts.go`, `server/server.go`, `server/reload.go`, `server/jetstream.go`, `server/mqtt.go`, `server/events.go` | §6.4–6.5 pass; `TestConfigCheck` extended; `nats-server -t` on the §7 configs succeeds. A server with a unix `advertise` cannot start until phase 4, so `-t` is the only runtime check here. |
| 3 | Listener: `listenRouteUnix`, stale-socket logic, `startRouteAcceptLoop` transport switch, self-route map, `ClusterUnixAddr`, `PortsInfo`. | `server/route.go`, `server/server.go`, `server/uds.go` | §6.6–6.7 pass; a single server starts and stops cleanly on a UDS and the file is gone after `Shutdown`. |
| 4 | Dial and gossip: `connectToRoute` branch, `processRouteInfo`, `processImplicitRoute`, `hasThisRouteConfigured`, `setRouteInfoHostPortAndIP`, TLS name fallback. | `server/route.go`, `server/client.go` | §6.8–6.9 and the in-process three-server mesh test pass with `-race`. |
| 5 | Monitoring: `udsStats`, `ClusterOptsVarz`, `RouteInfo`, `RouteStat`, ports file. | `server/monitor.go`, `server/events.go`, `server/server.go` | §6.10 pass; `/varz` and `/routez` JSON verified in the integration script. |
| 6 | Multi-process integration: configs, script, docs. | `test/configs/uds/*.conf`, `scripts/uds-cluster-smoke.sh`, this doc | §7 passes locally; script output pasted into the PR description as the containerd PR did. |
| 7 | PR: draft first, squash, `Signed-off-by`, human-written description with `ss -xp` evidence. | — | CI green on lint, linux amd64/386, windows matrix, race and no-race shards. |

Rough size: about 500 lines of non-test Go and 1,500 lines of tests.

## 6. Unit test plan

Conventions used throughout, going beyond the repo norm on purpose:

- Every table row has `description` and `expected` fields. `expected` is a struct or
  a string and the assertion is exact, never a bare "no error".
- Rows are grouped in the table as **positive**, **negative**, **boundary**, **corner**
  with a comment line between groups.
- `t.Run(tc.description, ...)`, `t.Helper()` in helpers, `t.Parallel()` where the test
  touches no global (`FlagSnapshot`, `natsListenConfig`, ports).
- Server options come from `defaultUnixClusterOptions(t)`, which starts from
  `DefaultOptions()` and replaces `Cluster.Port: -1` with `Port: 0` plus a `UnixSocket`
  from `tempSocketPath`. `nextServerOpts` also forces `Cluster.Port = -1`, so UDS tests
  build peers with the helper instead.
- Socket paths come from `tempSocketPath(t)`: a short directory under `os.TempDir()`
  cleaned up by `t.Cleanup`. `t.TempDir()` is **not** used because its path includes the
  test name and can exceed 104 bytes on macOS (`/var/folders/...`) and 108 on Linux for
  long subtest names. The helper fails the test if the path would exceed the limit.
- Tests that need a real UDS call `skipIfNoUnixSockets(t)`, which listens once and skips
  on `EAFNOSUPPORT`/`WSAEAFNOSUPPORT`, mirroring the containerd test.
- Names follow the runner's regex sharding: plain `TestRouteUnix*` / `TestUnix*` land in
  `srv_pkg_non_js_tests`; anything slow enough to matter is `TestNoRaceRouteUnix*` in a
  `norace_*_test.go` file.

### 6.1 `TestParseUnixAddr` (`server/uds_test.go`)

Row shape: `{description, in string, expected string, expectedErr string}`.

| Group | description | in | expected / expectedErr |
|---|---|---|---|
| positive | pathname socket | `unix:///run/nats/a.sock` | `/run/nats/a.sock` |
| positive | shortest absolute path | `unix:///a` | `/a` |
| positive | uppercase scheme accepted | `UNIX:///run/a.sock` | `/run/a.sock` |
| positive | mixed-case scheme accepted | `Unix:///run/a.sock` | `/run/a.sock` |
| positive | abstract socket | `unix://@nats-a` | `@nats-a` |
| positive | abstract name containing slash | `unix://@nats/a` | `@nats/a` |
| positive | windows drive path stored verbatim | `unix:///C:/nats/a.sock` | `/C:/nats/a.sock` |
| positive | windows lowercase drive | `unix:///c:/nats/a.sock` | `/c:/nats/a.sock` |
| positive | dots and dashes in path | `unix:///run/nats.io/a-b_c.sock` | `/run/nats.io/a-b_c.sock` |
| negative | percent sign rejected (canonical URL round-trip would re-encode it) | `unix:///run/a%20b.sock` | err `must not contain "%"` |
| negative | empty string | `` | err `must start with "unix://"` |
| negative | bare path without scheme | `/run/a.sock` | err `must start with "unix://"` |
| negative | tcp scheme | `tcp://127.0.0.1:6222` | err `must start with "unix://"` |
| negative | nats-route scheme | `nats-route://127.0.0.1:6222` | err `must start with "unix://"` |
| negative | scheme with one slash | `unix:/run/a.sock` | err `must start with "unix://"` |
| negative | scheme only | `unix://` | err `no socket path` |
| negative | relative path | `unix://run/a.sock` | err `must be an absolute path` |
| negative | trailing slash | `unix:///run/a.sock/` | err `not a directory` |
| negative | query string | `unix:///run/a.sock?x=1` | err `query` |
| negative | bare query delimiter | `unix:///run/a.sock?` | err `query` |
| negative | fragment | `unix:///run/a.sock#f` | err `fragment` |
| negative | bare fragment delimiter | `unix:///run/a.sock#` | err `fragment` |
| negative | empty abstract name | `unix://@` | err `no abstract socket name` |
| negative | userinfo not supported | `unix://ruser:pw@/run/a.sock` | err `must be an absolute path` (documents the §3.1 decision) |
| negative | leading space | ` unix:///run/a.sock` | err `leading or trailing spaces` |
| negative | trailing space | `unix:///run/a.sock ` | err `leading or trailing spaces` |
| negative | embedded newline | `unix:///run/a.sock\nx` | err `unable to parse` |
| negative | malformed percent escape | `unix:///%ZZ` | err `unable to parse` |
| negative | NUL byte in path | `unix:///run/a\x00.sock` | err `unable to parse` |
| boundary | path exactly at limit | `unix://` + `/` + `a`×(max−1) | that path |
| boundary | path one byte over limit | `unix://` + `/` + `a`×max | err `too long` |
| boundary | abstract name at limit (`@` counts as the NUL byte) | `unix://@` + `a`×(max−1) | that name |
| boundary | abstract name one over | `unix://@` + `a`×max | err `too long` |
| boundary | windows drive form at limit (native is one byte shorter) | `unix:///C:/` + `a`×(max−3) | accepted on windows build, `too long` elsewhere (asserted per `runtime.GOOS`) |
| corner | double slash inside path preserved | `unix:///run//a.sock` | `/run//a.sock` |
| corner | dot segments preserved, not cleaned | `unix:///run/../a.sock` | `/run/../a.sock` |
| corner | unicode path | `unix:///run/ñ.sock` | `/run/ñ.sock` |
| corner | multibyte path measured in bytes not runes | `unix://` + `/` + `ñ`×(max/2) | err `too long` |
| corner | four slashes | `unix:////run/a.sock` | `//run/a.sock` (absolute, allowed; kernel treats as `/run/a.sock`) |

### 6.2 `TestNativeUnixAddr` (`uds_test.go`, `//go:build !windows`) and `TestNativeUnixAddrWindows` (`uds_windows_test.go`)

POSIX rows assert identity for every input in 6.1's positive group. Windows rows
(`{description, in, expected}`):

| description | in | expected |
|---|---|---|
| uppercase drive | `/C:/ProgramData/nats/a.sock` | `C:\ProgramData\nats\a.sock` |
| lowercase drive | `/c:/x/a.sock` | `c:\x\a.sock` |
| non-C drive | `/D:/a.sock` | `D:\a.sock` |
| shortest drive path | `/C:/a` | `C:\a` |
| drive-less absolute keeps leading separator | `/run/a.sock` | `\run\a.sock` |
| letter without colon is not a drive | `/abc/a.sock` | `\abc\a.sock` |
| abstract passes through | `@nats-a` | `@nats-a` |
| abstract with slash untouched | `@nats/a` | `@nats/a` |
| UNC-looking path | `//server/share/a.sock` | `\\server\share\a.sock` |

### 6.3 `TestUnixRouteURLRoundTrip`

Row shape `{description, in string, expectedAddr string, expectedString string}`. Verifies
`unixRouteURL(parseUnixAddr(in)).String()` and that `unixAddrFromRouteURL` recovers the
address from (a) the canonical URL, (b) `url.Parse(in)`, (c) `url.Parse(canonical.String())`.
Rows: pathname, abstract, windows drive, and a negative row for `nats-route://` returning
`ok == false`, plus a corner row for `url.Parse("unix://@name")` landing in `User`/`Host`.

### 6.4 `TestClusterOptsUnixSocketConfig` (`server/opts_uds_test.go`)

Row shape `{description, config string, expected ClusterOpts fields, expectedErr string}`
using `createConfFile` + `ProcessConfigFile`.

| Group | description | config fragment | expected |
|---|---|---|---|
| positive | listen unix sets UnixSocket and leaves host/port zero | `cluster { listen: "unix:///run/a.sock" }` | `UnixSocket=/run/a.sock, Host="", Port=0` |
| positive | abstract listen | `cluster { listen: "unix://@a" }` | `UnixSocket=@a` |
| positive | unix routes canonicalised | `routes: ["unix:///run/b.sock", "unix://@c"]` | `Routes[0]={unix,/run/b.sock}`, `Routes[1]={unix,@c}` |
| positive | mixed tcp and unix routes | `routes: ["nats-route://127.0.0.1:6222", "unix:///run/b.sock"]` | both preserved in order |
| positive | unix advertise with unix listener | `listen: "unix:///run/a.sock", advertise: "unix:///run/p/a.sock"` | `Advertise="unix:///run/p/a.sock"` |
| positive | tcp cluster untouched | `cluster { listen: 127.0.0.1:6222 }` | `UnixSocket=""`, `Port=6222` |
| negative | unix listen plus port | `listen: "unix:///run/a.sock", port: 6222` | err `mutually exclusive` |
| negative | unix listen plus host | `listen: "unix:///run/a.sock", host: 127.0.0.1` | err `mutually exclusive` |
| negative | unix advertise with tcp listener | `listen: 127.0.0.1:6222, advertise: "unix:///run/a.sock"` | err `advertise transport` |
| negative | tcp advertise with unix listener | `listen: "unix:///run/a.sock", advertise: "10.0.0.1:6222"` | err `advertise transport` |
| negative | invalid unix route | `routes: ["unix://run/b.sock"]` | err `must be an absolute path` at the route token line/pos |
| negative | invalid unix listen | `listen: "unix:///run/a.sock/"` | err `not a directory` |
| negative | duplicate unix route | `routes: ["unix:///run/b.sock", "unix:///run/b.sock"]` | warning `Duplicate route entry detected` |
| boundary | listen path at OS limit | generated | accepted |
| boundary | listen path over OS limit | generated | err `too long` |
| corner | unquoted unix listen (the conf lexer does not treat `//` after `:` as a comment) | `listen: unix:///run/a.sock` | `UnixSocket=/run/a.sock`; quoting is optional |
| corner | scheme case in config | `listen: "UNIX:///run/a.sock"` | `UnixSocket=/run/a.sock` |

`TestConfigCheck` (`server/config_check_test.go`) gains the negative rows above with
exact `errorLine`/`errorPos`.

### 6.5 `TestClusterUnixFlags` (`server/opts_uds_test.go`)

Row shape `{description, args []string, expected ClusterOpts / Routes, expectedErr}` via
`ConfigureOptions`:

| description | args | expected |
|---|---|---|
| -cluster unix | `-cluster unix:///run/a.sock` | `UnixSocket=/run/a.sock` |
| -cluster unix overrides config tcp listen | `-c cluster_tcp.conf -cluster unix:///run/a.sock` | `UnixSocket` set, `Port=0` |
| -routes unix list | `-routes unix:///run/b.sock,unix://@c` | two canonical URLs |
| -routes with spaces around commas | `-routes "unix:///run/b.sock, unix://@c"` | trimmed |
| -cluster_advertise unix | `-cluster unix:///run/a.sock -cluster_advertise unix:///p/a.sock` | `Advertise` set |
| invalid -routes surfaces error | `-routes unix://relative` | err `must be an absolute path` (today this would be silently dropped) |
| -cluster random-port suffix is TCP-only syntax | `-cluster unix:///run/a.sock:-1` | `UnixSocket=/run/a.sock:-1` is rejected by `listenRouteUnix` at start (`ENOENT`/`EINVAL`); `overrideCluster` must not apply its `:-1` rewrite to `unix://` values, asserted by checking `ListenStr` is untouched |
| -routes without -cluster | `-routes unix:///run/b.sock` | err `solicited routes require cluster capabilities` |
| -routes with -cluster unix | `-cluster unix:///run/a.sock -routes unix:///run/b.sock` | accepted (the `opts.go:6477` guard recognises a UDS listener) |

### 6.6 `TestListenRouteUnixStaleSocket` (`server/uds_test.go`, needs real sockets)

Row shape `{description, setup func(path), expected string (listening|error substring), expectedStaleRemoved uint64}`:

| Group | description | setup | expected |
|---|---|---|---|
| positive | fresh path | none | listening, stale=0 |
| positive | stale socket file from dead process | listen then `Close` with `SetUnlinkOnClose(false)` | listening, stale=1, Warnf logged |
| negative | live socket held by another listener | leave a listener open | err `in use by another process`, other listener unaffected |
| negative | regular file at path | `os.WriteFile` | err `is not a socket`, file still present |
| negative | directory at path | `os.Mkdir` | err `is not a socket` |
| negative | parent directory missing | `/nonexistent/x/a.sock` | error from `net.Listen`, no removal |
| negative | parent directory not writable (skip as root) | `chmod 0500` | `EACCES` from `net.Listen` |
| boundary | path exactly at limit | generated | listening |
| corner | dangling symlink at path | `os.Symlink` | err `is not a socket` (Lstat, so the link is not followed) |
| corner | abstract socket (linux only) | `@name` | listening, no file created |
| corner | listener closed removes file | listen, `Close` | `os.Lstat` returns `ENOENT` |

### 6.7 `TestServerUnixListenerAccessors`

Row shape `{description, opts func() *Options, expected struct{ clusterAddrNil bool; unixAddr string; ports []string; healthz int }}`:

| description | expected |
|---|---|
| tcp listener | `ClusterAddr != nil`, `ClusterUnixAddr == nil`, ports `nats-route://ip:port`, healthz 200 |
| unix listener | `ClusterAddr == nil`, `ClusterUnixAddr.Name == path`, ports `unix:///...`, healthz 200 |
| unix listener bind failure (regular file at path) | `NewServer` ok, `Start` → `routeListenerErr` set, healthz 503 with `route` error |
| no cluster | both nil, ports has no cluster entry |

### 6.8 `TestRouteInfoUnix` (INFO generation and consumption)

`setRouteInfoHostPortAndIP` and `processRouteInfo` are exercised with a table
`{description, listener (tcp|unix), advertise, expected Info{Host,Port,IP}}`:

| description | listener | advertise | expected |
|---|---|---|---|
| tcp no advertise | tcp 127.0.0.1:0 | `` | `Host=127.0.0.1, Port=<bound>, IP=""` |
| tcp with advertise | tcp | `10.0.0.1:6222` | `Host=10.0.0.1, Port=6222, IP=nats-route://10.0.0.1:6222/` |
| unix no advertise | unix | `` | `Host="", Port=0, IP=""` |
| unix with advertise | unix | `unix:///p/a.sock` | `Host="", Port=0, IP=unix:///p/a.sock` |
| unix with abstract advertise | unix | `unix://@a` | `IP=unix://@a` |

`TestHasThisRouteConfiguredUnix` table `{description, configuredRoutes, info Info, expected bool}`
covers: exact path match, abstract match, path differs by trailing component, tcp
configured vs unix info, unix configured vs tcp info, empty routes, case-sensitive path
comparison (paths are case-sensitive on Linux; a corner row documents this), and a
canonical-vs-parsed `unix://@name` match.

`TestProcessImplicitRouteUnix` table `{description, info Info, expected (dialAttempted bool)}`:
no address → no dial; unix IP not configured → dial attempted once; unix IP configured →
no dial; own ID → no dial; unix IP equal to own listen path → no dial (self map).

### 6.9 `TestRouteUnixDial` (`server/routes_uds_test.go`, real sockets)

Row shape `{description, target func() string, expected string}`:

| Group | description | expected |
|---|---|---|
| positive | dial a listening path | connected, INFO received |
| positive | dial abstract (linux) | connected |
| negative | dial nonexistent path | `DialErrors` incremented, error contains path, Explicit route keeps retrying, Implicit gives up after `connect_retries` |
| negative | dial a regular file | `ECONNREFUSED`/`ENOTSOCK` surfaced |
| boundary | `connect_retries: 0` implicit | exactly one attempt |
| boundary | `connect_backoff: true` | delays double up to `routeConnectMaxDelay` (use the existing `routeConnectDelay` test override) |
| corner | route removed by reload mid-retry | goroutine exits, `routeStillValid` false |
| corner | dial own listen path explicitly | no connection, no error log, `Active` unchanged |
| corner | peer closes immediately after accept | reconnect scheduled via `reConnectToRoute` |

### 6.10 `TestRouteUnixMonitoring`

After forming a two-server UDS cluster (`checkClusterFormed`), assert with a table over
`/varz`, `/routez`, `PortsInfo` and `STATSZ`:

| description | endpoint | expected |
|---|---|---|
| varz reports listener | `/varz` | `cluster.unix_socket == path`, `cluster.cluster_port` absent, `cluster.addr` absent |
| varz counters | `/varz` | `accepted=1` on the acceptor, `dialed=1` on the dialer, `active=1` on both, `dial_errors=0` |
| routez transport | `/routez` | every route has `transport == "unix"`, `ip == ""`, `port == 0`; solicited side has `unix_socket == path` |
| routez tcp regression | `/routez` on a TCP pair | `transport == "tcp"`, `ip`/`port` populated as before |
| statsz | `$SYS.REQ.SERVER.PING.STATSZ` | `routes[].transport == "unix"` |
| ports file | `PortsInfo` | `Cluster == ["unix:///..."]` |
| counters after peer restart | restart dialer | `accepted=2`, `active=1` |
| counters after stale removal | pre-create stale file | `stale_removed=1` |

Varz JSON is also checked for field ordering/omission with `json.Marshal` against a
golden fragment so that a TCP-only server's `/varz` is byte-identical to today.

### 6.11 `TestRouteUnixReload`

Table `{description, initial conf, reloaded conf, expected error or state}`: adding a
unix route triggers a dial; removing one closes it with `RouteRemoved`; changing
`listen` from unix to tcp is rejected; changing the unix path is rejected; changing
advertise between unix paths is accepted and the new INFO is forwarded.

### 6.12 `TestRouteUnixTLS`

Table over `{description, tlsName present, expected}`: solicit with configured TCP
hostname route present → handshake uses it; no hostname anywhere → the §4.6 error;
`insecure: true` → connects with the existing insecure warning.

## 7. Integration testing plan

### 7.1 In-process three-server full mesh (`server/routes_uds_test.go`)

`TestRouteUnixThreeServerMesh` (and a `TestNoRaceRouteUnixThreeServerMeshTraffic`
variant that moves 100k messages) starts servers A, B, C with:

```
A: listen unix:///$D/a.sock  routes [unix:///$D/b.sock, unix:///$D/c.sock]
B: listen unix:///$D/b.sock  routes [unix:///$D/a.sock, unix:///$D/c.sock]
C: listen unix:///$D/c.sock  routes [unix:///$D/a.sock, unix:///$D/b.sock]
```

The test lives in the `server` package so it can inspect `s.routes` and the counters;
its `checkClusterFormed` (`server/routes_test.go:316`) is pool-size aware, so the
assertions below hold with the default `pool_size` as well as with `pool_size: -1`. A
black-box copy in the `test` package (where `test.RunServer` forces `PoolSize = -1`)
reuses `test/cluster_test.go`'s `runThreeServers` shape with the UDS configs from §7.2.

Assertions: `checkClusterFormed`; `checkNumRoutes == 2` on each; a subscriber on A
receives a message published on C; `/routez` on each shows `transport == "unix"`; the
duplicate-route dance (A dials B while B dials A) resolves to exactly one route per pair
as it does for TCP; shutting down B and restarting it on the same path re-forms the mesh
within the route reconnect delay; after `Shutdown` of all three no socket files remain.

A second table-driven variant, `TestRouteUnixMeshTopologies`, runs the mesh in
`{description, topology}` rows: full explicit mesh; ring (A→B, B→C, C→A) with no
advertise (mesh still forms because each pair has one explicit route); ring with
advertise set (gossip fills nothing new but must not create duplicate routes); mixed
transport where A↔B is TCP and B↔C, C↔A are UDS.

### 7.2 Three-process smoke test (`scripts/uds-cluster-smoke.sh`, `test/configs/uds/`)

Purpose: prove the feature with real processes, and be the harness later reused with the
RDMA proxy. Shape follows the containerd PR's evidence-driven test section.

Config files `test/configs/uds/srv_{a,b,c}.conf`:

```hcl
# Cluster Server A
server_name: uds-a
listen: 127.0.0.1:14222
http: 127.0.0.1:18222
cluster {
  name: uds
  listen: "unix:///tmp/nats-uds/a.sock"
  routes = [ "unix:///tmp/nats-uds/b.sock", "unix:///tmp/nats-uds/c.sock" ]
}
no_sys_acc: true
```

Script steps (bash, `set -euo pipefail`, no external deps beyond `curl`, `ss`, and the
`nats` CLI which is fetched via `nix shell nixpkgs#natscli` when missing):

1. `go build -o "$OUT/nats-server" .` and print `nats-server --version`.
2. `nats-server -t -c` each config.
3. Start A, B, C in the background with `-l "$OUT/<name>.log"`; record PIDs.
4. Wait up to 10 s until each `/routez` reports `num_routes == 2` and every route has
   `"transport":"unix"`.
5. Evidence: `ss -xlp | grep nats-uds` shows three `LISTEN` sockets; `ss -xp | grep
   nats-uds` shows the established pairs owned by the `nats-server` PIDs; `/varz` shows
   `unix_socket_stats`.
6. Traffic: `nats sub` on A, `nats pub` on C, assert delivery; `nats bench` for a
   throughput number to paste into the PR.
7. Kill B with `SIGKILL` so its socket file is left behind, restart it, wait for
   `num_routes == 2` again and grep the log for `Removed stale route unix socket`.
8. `SIGTERM` all three, assert `/tmp/nats-uds` contains no `.sock` files.
9. Print a PASS/FAIL summary; exit non-zero on any failure. Logs stay in `$OUT` for the
   PR description.

### 7.3 Proxied topology (manual, with `uds-over-rdma-proxy`)

The same script with an environment switch `UDS_PAIRS=1` uses **three UDS pairs**, one
per edge of the mesh, so that each NATS instance opens exactly the sockets it would in a
real two-host deployment:

```
A: listen a.sock   routes [ ab.sock ]      # ab.sock is the proxy's client end for pair A→B
B: listen b.sock   routes [ bc.sock ]
C: listen c.sock   routes [ ca.sock ]
proxy pairs:  ab.sock -> b.sock,  bc.sock -> c.sock,  ca.sock -> a.sock
```

Because no server advertises, the mesh is formed purely by the three explicit routes,
which is exactly what §7.1's ring topology row asserts in-process. On a single machine
the proxy can be replaced by `socat UNIX-LISTEN:ab.sock,fork UNIX-CONNECT:b.sock` for a
dry run; the acceptance evidence is the same `ss -xp` output showing the proxy, not the
peer server, on the far end of each NATS socket.

### 7.4 CI

- The unit and in-process tests run in the existing `server-pkg-non-js` and
  `no-race-*` shards; nothing new is added to `scripts/runTestsOnTravis.sh`.
- `build-windows` compiles the windows files; `GOOS=windows go vet ./server/` is run
  locally before pushing. The Windows-only table (§6.2) is a `_windows_test.go` file so
  it also executes on the windows runners.
- The multi-process script is not wired into CI in this PR (it needs `ss` and the CLI);
  its output is attached to the PR.

## 8. Cross-platform notes

| Platform | Pathname | Abstract | `sun_path` | Notes |
|---|---|---|---|---|
| Linux | yes | yes | 108 | Primary target (RDMA proxy). |
| macOS / FreeBSD / OpenBSD / NetBSD | yes | no (runtime error) | 104 | `t.TempDir()` paths are long; use `tempSocketPath`. |
| Solaris / illumos | yes | no | 108 | Compile-only coverage today. |
| Windows 10 1803+ / Server 2019+ | yes | Go translates `@` but the kernel rejects it | 108 | URL form `unix:///C:/...`; `WSAECONNREFUSED` for the stale probe. |
| wasm | n/a | n/a | constant 108 | Package must still compile; listener creation errors at runtime like every other socket. |

## 9. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Panics from `(*net.TCPAddr)` assertions on the new listener type. | Every site is listed in §2 and replaced by a type switch; a unit test starts a UDS server and calls every accessor. |
| Behaviour change for TCP clusters. | No wire change when `UnixSocket` is empty; golden `/varz` JSON test; full existing route suite runs unchanged. |
| Old servers in a mixed cluster mis-handle `Port=0` INFO. | Documented as unsupported. No route `Proto` bump is proposed; if maintainers prefer one, a UDS-listening server can refuse routes from peers with an older `Proto` with a clear error. |
| Stale-socket removal deleting something it should not. | Only removes if `Lstat` says socket **and** a dial gets `ECONNREFUSED`; never follows symlinks; covered by §6.6. |
| Long temp paths in tests on macOS. | `tempSocketPath` helper with a hard assertion. |
| Route TLS with no hostname. | Explicit error message, §6.12. |
| Exporter shows nothing for the new fields. | Follow-up PR to `prometheus-nats-exporter`; fields are stable JSON. |

## 10. Contributor checklist (from `CONTRIBUTING.md`)

- [ ] Issue opened first, linking this document; wait for maintainer feedback.
- [ ] Draft PR while in progress; mark ready only when CI is green.
- [ ] Every commit carries `Signed-off-by: David Seddon <dave.seddon.ca@gmail.com>` (`git commit -s`).
- [ ] Rebased on latest `main`; squashed to one commit per phase or one overall.
- [ ] Tests included for the new feature (required for acceptance); no new `go.mod` entries.
- [ ] PR description written by a human, with the `ss -xp` and `/routez` evidence from §7.2.
- [ ] `golangci-lint run` (v2.13.0 config in `.golangci.yml`) and `GOOS=windows go vet ./server/` clean.

## 11. Follow-ups

- `prometheus-nats-exporter`: map `cluster.unix_socket_stats.*` and `routez.routes[].transport`.
- Client and leafnode listeners over UDS (reuse `server/uds.go` unchanged).
- Optional `unix_socket_mode` for the listener; today directory permissions are the control.
- Dual TCP + UDS route listeners once "Multiple listen endpoints" lands.
- Route authentication credentials inside `unix://` URLs, if maintainers want URL parity.
