# Status: Cluster routes over Unix domain sockets

Tracks implementation of [uds-cluster-routes.md](uds-cluster-routes.md). Update this file
at the end of every working session. Section numbers (§) refer to the design doc.

Legend: `[ ]` not started · `[~]` in progress · `[x]` done · `[!]` blocked

Last updated: 2026-09-22

## Summary

| Phase | Scope | State | Notes |
|---|---|---|---|
| 0 | GitHub issue | `[ ]` | Not opened yet. Write it by hand (CONTRIBUTING AI policy). |
| 1 | Address parsing, canonical URL helpers, per-OS files | `[x]` | Committed on `uds-cluster-routes`. |
| 2 | Options, flags, validation, reload rejection | `[x]` | Committed. |
| 3 | Listener, stale socket, self-route map, accessors | `[~]` | |
| 4 | Dial, gossip, INFO, TLS name fallback | `[ ]` | |
| 5 | Monitoring fields and counters | `[ ]` | |
| 6 | Multi-process configs, script, docs | `[ ]` | |
| 7 | PR: draft, squash, sign-off, evidence | `[ ]` | |

Branch: `uds-cluster-routes` (from `main` at `edb1b17a`)

## Phase 0: issue

- [ ] Issue opened, number: `#____`
- [ ] Design doc linked from the issue
- [ ] Maintainer response received

## Phase 1: parsing and platform helpers (§4.1, §4.2)

Files: `server/uds.go`, `server/uds_unix.go`, `server/uds_windows.go`,
`server/uds_sockaddr.go`, `server/uds_sockaddr_wasm.go`

- [x] `maxUnixSocketPathLen` per platform (`!wasm` from `syscall.RawSockaddrUnix`, `wasm` constant 108)
- [x] `nativeUnixAddr` POSIX identity, Windows drive-letter and separator conversion
- [x] `parseUnixAddr` with all §3.1 validation rules
- [x] `unixRouteURL`, `unixAddrFromRouteURL`, `isUnixRouteURL` (handles `url.Parse` User/Host form for `@name`)
- [x] Copyright headers and `//go:build` placement match `disk_avail_windows.go`

Tests:

- [x] §6.1 `TestParseUnixAddr` (all four groups)
- [x] §6.2 `TestNativeUnixAddr` and `TestNativeUnixAddrWindows`; `TestIsConnRefused` per platform; `TestMaxUnixSocketPathLen`
- [x] §6.3 `TestUnixRouteURLRoundTrip`; `TestUnixAddrFromRouteURLRejects`
- [x] `GOOS=windows go vet ./server/` clean
- [x] `GOOS=js GOARCH=wasm go build ./server/` compiles (`go vet` fails on pre-existing `signal_test.go` `SIGUSR1`, unrelated)

## Phase 2: options and validation (§3.2, §4.5)

Files: `server/opts.go`, `server/server.go`, `server/reload.go`, `server/events.go`, `server/jetstream.go`, `server/mqtt.go`; tests in `server/opts_uds_test.go` and `server/config_check_test.go`

- [x] `ClusterOpts.UnixSocket` field with doc comment and `json:"-"` (ClusterOpts json tags are deprecated)
- [x] `(*ClusterOpts).listenEnabled()` helper
- [x] Replace `Cluster.Port != 0` / `== 0` at all twelve sites (list in §2; tick each below)
  - [x] `server.go:815` leafnode remotes check
  - [x] `server.go:881` dynamic cluster name
  - [x] `server.go:1135` validateCluster leaf check
  - [x] `server.go:1578` standAloneMode
  - [x] `server.go:1952` shouldTrackSubscriptions
  - [x] `server.go:2556` StartRouting launch
  - [x] `server.go:4008` healthz route check
  - [x] `server.go:4413` serviceListeners
  - [x] `events.go:914` system events gate
  - [x] `jetstream.go:2974` clustered JetStream gate
  - [x] `mqtt.go:721` and `mqtt.go:750` MQTT cluster checks
  - [x] `opts.go:6042` cluster defaults gate
  - [x] `opts.go:6477` "solicited routes require cluster capabilities"
- [x] `parseCluster` `listen` accepts `unix://` string before `parseListen`
- [x] `parseURL` canonicalises `unix://` for `typ == "route"`
- [x] `RoutesFromStr` error-returning sibling used by flag processing; exported signature unchanged
- [x] `overrideCluster` handles `-cluster unix://...` and skips the `:-1` rewrite for it
- [x] `advertise` accepts `unix://`; transport must match listener
- [x] `validateCluster`: mutual exclusion with host/port, advertise transport match, non-Linux abstract warning
- [x] `validateClusterOpts` (reload): `UnixSocket` non-reloadable; `Advertise` validated with `parseUnixAddr` when `unix://`
- [x] `setBaselineOptions` only defaults `Cluster.Host` when `UnixSocket` is empty

Tests:

- [x] §6.4 `TestClusterOptsUnixSocketConfig`
- [x] `TestConfigCheck` rows with exact `errorLine`/`errorPos`
- [x] §6.5 `TestClusterUnixFlags`
- [x] §6.11 rejection rows as `TestValidateClusterOptsUnixSocket` (unit table over `validateClusterOpts`); live add/remove rows land in phase 4 as `TestRouteUnixReload`
- [x] `nats-server -t -c` on a UDS config passes and rejects `listen: unix` + `port` with line:col
- [x] Extra tables: `TestRoutesFromStrUnix`, `TestClusterListenEnabled`, `TestSetBaselineOptionsUnixSocket`, `TestValidateClusterUnixSocket`, `TestClusterPortPredicateSites` (greps non-test `server/*.go`)

## Phase 3: listener (§4.3, §4.7)

Files: `server/route.go`, `server/server.go`, `server/uds.go`

- [ ] `listenRouteUnix` with stale-socket sequence (Lstat, not-a-socket error, probe dial, `ECONNREFUSED` remove)
- [ ] `isConnRefused` per platform (`syscall.ECONNREFUSED`, `windows.WSAECONNREFUSED`)
- [ ] `startRouteAcceptLoop` transport switch; both `(*net.TCPAddr)` assertions replaced by a type switch
- [ ] Notice line for unix socket
- [ ] `s.unixRoutesToSelf` populated with native listen path and advertise path
- [ ] `ClusterAddr()` returns nil for UDS; add `ClusterUnixAddr()` and `ClusterListenAddr()`
- [ ] `resolveHostPorts` / `formatURL` type switch; `PortsInfo` emits `unix:///...`
- [ ] `client.go:785` sets `c.host` from `*net.UnixAddr`
- [ ] Verified socket file is gone after `Shutdown` (Linux and Windows CI)

Tests:

- [ ] §6.6 `TestListenRouteUnixStaleSocket`
- [ ] §6.7 `TestServerUnixListenerAccessors`
- [ ] `tempSocketPath(t)` and `skipIfNoUnixSockets(t)` helpers

## Phase 4: dial and gossip (§4.4, §4.6)

Files: `server/route.go`, `server/client.go`

- [ ] `connectToRoute` unix branch (`dialRouteUnix`, self check, no resolver)
- [ ] `setRouteInfoHostPortAndIP`: UDS listener gives `Host=""`, `Port=0`, `IP` only when advertise set
- [ ] `processRouteInfo`: switch on `RemoteAddr().(type)`; no `nats-route://:0/` construction
- [ ] `processImplicitRoute`: early return when no dialable address; unix path dial when advertised
- [ ] `hasThisRouteConfigured`: UDS address comparison via `unixAddrFromRouteURL`
- [ ] `saveRouteTLSName`: skips UDS URLs explicitly
- [ ] `doTLSHandshake` ServerName fallback and explicit error for UDS with no name
- [ ] Audit every `c.route.url` dereference for nil on accept-side UDS routes (12 sites, `grep -n "route.url" server/*.go`)

Tests:

- [ ] §6.8 `TestRouteInfoUnix`, `TestHasThisRouteConfiguredUnix`, `TestProcessImplicitRouteUnix`
- [ ] §6.9 `TestRouteUnixDial`
- [ ] §6.11 `TestRouteUnixReload` add/remove rows
- [ ] §6.12 `TestRouteUnixTLS`
- [ ] §7.1 `TestRouteUnixThreeServerMesh`, `TestRouteUnixMeshTopologies`
- [ ] §7.1 `TestNoRaceRouteUnixThreeServerMeshTraffic` in a `norace_*_test.go`
- [ ] Full existing route suite passes with `-race`: `go test -race -run 'TestRoute' ./server/`

## Phase 5: monitoring (§3.3)

Files: `server/server.go`, `server/monitor.go`, `server/events.go`

- [ ] `udsStats` struct embedded in `Server` at the aligned head, `atomic.Uint64` fields
- [ ] Increments at accept, dial success, dial failure, stale removal
- [ ] `RouteUnixSocketStats` type; `ClusterOptsVarz.UnixSocket` and `.UnixSocketStats`
- [ ] `updateVarzRuntimeFields` copies counters; `Active` via `forEachRoute`
- [ ] `RouteInfo.Transport` and `.UnixSocket`; `Routez` switch on `RemoteAddr().(type)`
- [ ] `RouteStat.Transport` in STATSZ
- [ ] Inline `//` doc comment on every new field (monitor.go convention)

Tests:

- [ ] §6.10 `TestRouteUnixMonitoring`
- [ ] Golden `/varz` JSON for a TCP-only server unchanged

## Phase 6: multi-process integration (§7.2, §7.3)

Files: `test/configs/uds/srv_{a,b,c}.conf`, `scripts/uds-cluster-smoke.sh`, docs

- [ ] Three config files
- [ ] Script: build, config-check, start, wait for `num_routes == 2` and `transport == "unix"`
- [ ] Script: `ss -xlp` / `ss -xp` evidence capture
- [ ] Script: pub/sub and `nats bench` traffic
- [ ] Script: SIGKILL B, restart, stale-socket log line, mesh re-forms
- [ ] Script: SIGTERM all, assert no `.sock` files remain
- [ ] `UDS_PAIRS=1` mode with `socat` stand-in for the proxy
- [ ] Run against the real `uds-over-rdma-proxy` (manual); paste evidence below
- [ ] Config reference / README mention of `unix://` listen and routes

## Phase 7: PR

- [ ] Branch rebased on latest `main`
- [ ] One commit per phase or one overall, each `Signed-off-by: David Seddon <dave.seddon.ca@gmail.com>`
- [ ] `golangci-lint run` clean
- [ ] Draft PR opened, number: `#____`
- [ ] PR description written by hand with `ss -xp`, `/routez` and bench evidence
- [ ] CI green: lint, linux amd64/386, windows-2022/2025, race and no-race shards
- [ ] Marked ready for review
- [ ] Follow-up issue filed on `prometheus-nats-exporter` for the new fields

## Open questions

Record decisions here with the date so they are not re-litigated.

| Date | Question | Decision |
|---|---|---|
| 2026-09-22 | Credentials inside `unix://` URLs? | No. `@` collides with abstract marker; use cluster `authorization {}` (§3.1). Revisit if maintainers ask. |
| 2026-09-22 | Advertise listen path by default for UDS? | No. Paths are namespace-local; explicit routes form the mesh (§4.4). |
| 2026-09-22 | Route `Proto` bump? | Not proposed; documented as unsupported for mixed old/new clusters (§9). |
| | TLS over UDS on the dial side when no hostname exists? | Proposed: explicit error (§4.6). Awaiting maintainer view. |
| | `unix_socket_mode` option in v1? | Proposed: no, directory permissions (§4.3.1). |

## Evidence log

Paste command output that will go into the PR description here as it is produced.

Phase 2 (2026-09-22), `nats-server -t`:

```
$ nats-server -t -c uds-a.conf
nats-server: configuration file uds-a.conf is valid (sha256:dd74e37f...)
$ nats-server -t -c uds-bad.conf     # listen: "unix:///tmp/nats-uds/a.sock" + port: 6222
nats-server: uds-bad.conf:2:3: unix socket listen and host/port are mutually exclusive
```

Pre-existing on `main` (`edb1b17a`), not caused by this branch: `TestRouteSlowConsumerRecover`
fails on this machine (`Expected Slow Consumer routes`, bandwidth-shaping proxy timing).

## Session log

| Date | Work done | Next |
|---|---|---|
| 2026-09-22 | Codebase survey; design doc written; this status file created. | Open issue (phase 0); start phase 1 helpers and §6.1 table. |
| 2026-09-22 | Plan validated against code; design doc corrected (json tag, `setBaselineOptions` Host guard, 12 predicate sites, reject `%`, TLS error wording). Phase 1 helpers, per-OS files and tests written; `-race` green; windows vet, wasm/darwin/freebsd builds ok. Committed. | Phase 2: `UnixSocket` option, `listenEnabled()`, parse/validate/reload, predicate sites, tests. |
| 2026-09-22 | Phase 2 done: `UnixSocket` field, `listenEnabled()` at all 12 sites, parse-time and `validateCluster` transport rules, `routesFromStr` error variant for `-routes`, `overrideCluster` unix branch, reload rejection. Fixed phase-1 length rule (pathname limit is `sun_path`-1, abstract is `sun_path`, matching Go's `SockaddrUnix`). Found the conf lexer accepts unquoted `unix:///...`. `-race` green on config/options/reload sets; `./test` route suite green. Committed. | Phase 3: `listenRouteUnix`, `startRouteAcceptLoop` transport switch, accessors, `unixRoutesToSelf`, real-socket tests. |
