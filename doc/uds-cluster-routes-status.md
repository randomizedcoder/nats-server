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
| 3 | Listener, stale socket, self-route map, accessors | `[x]` | Committed. |
| 4 | Dial, gossip, INFO, TLS name fallback | `[x]` | Committed. `sendRouteConnect` cluster-auth fallback added for explicit unix routes. |
| 5 | Monitoring fields and counters | `[~]` | |
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

Files: `server/route.go`, `server/server.go`, `server/client.go`, `server/uds.go`, `server/uds_unix.go`, `server/uds_windows.go`; tests in `server/routes_uds_test.go`

- [x] `listenRouteUnix` with stale-socket sequence (Lstat, not-a-socket error, probe dial, `ECONNREFUSED` remove)
- [x] `isConnRefused` per platform (`syscall.ECONNREFUSED`, `windows.WSAECONNREFUSED`)
- [x] `startRouteAcceptLoop` transport switch; both `(*net.TCPAddr)` assertions replaced by a type switch
- [x] Notice line for unix socket
- [x] `s.unixRoutesToSelf` populated with the URL-form listen path and advertise path
- [x] `ClusterAddr()` returns nil for UDS; add `ClusterUnixAddr()` and `ClusterListenAddr()`
- [x] `resolveHostPorts` / `formatURL` type switch; `PortsInfo` emits `unix:///...`
- [x] `client.go:785` sets `c.host` from `*net.UnixAddr`
- [x] Verified socket file is gone after `Shutdown` (Linux locally; Windows pending CI)

Tests:

- [x] §6.6 `TestListenRouteUnixStaleSocket`
- [x] §6.7 `TestServerUnixListenerAccessors`
- [x] `tempSocketPath(t)`, `skipIfNoUnixSockets(t)`, `defaultUnixClusterOptions(t)`, `leaveStaleSocket(t)` helpers
- [x] Extra: `TestListenRouteUnixPathLength` (real bind at `sun_path`-1 and one over), `TestServerUnixListenerStartupErrors` (`routeListenerErr` set, no listener), `TestFormatURLUnix` (fake listener table), `TestURLUnixAddr` per OS

## Phase 4: dial and gossip (§4.4, §4.6)

Files: `server/route.go`, `server/client.go`

- [x] `connectToRoute` unix branch (self check against `unixRoutesToSelf`, `natsDialTimeout("unix", ...)`, no resolver, `dialed`/`dialErrors` counters)
- [x] `setRouteInfoHostPortAndIP`: UDS listener gives `Host=""`, `Port=0`, `IP` only when advertise set
- [x] `processRouteInfo`: switch on `RemoteAddr().(type)`; no `nats-route://:0/` construction; unix `info.IP` becomes the route URL
- [x] `processImplicitRoute`: early return when no dialable address; unix path dial when advertised
- [x] `hasThisRouteConfigured`: UDS address comparison via `unixAddrFromRouteURL`
- [x] `saveRouteTLSName`: skips UDS URLs explicitly
- [x] `doTLSHandshake` ServerName fallback and explicit error for UDS with no name (`errRouteTLSUnixNoName`, connection closed so the route retries)
- [x] Audit every `c.route.url` dereference for nil on accept-side UDS routes (16 sites; all solicit-only, nil-checked, or nil-safe `Redacted()`; the one accept-side site in `processRouteInfo` is nil-checked)
- [x] `sendRouteConnect`: explicit `unix://` routes without user-info fall back to the dialing server's cluster `authorization {}` credentials (found by `TestProcessImplicitRouteUnix`; §3.1 updated)

Tests:

- [x] §6.8 `TestRouteInfoUnix`, `TestHasThisRouteConfiguredUnix`, `TestProcessImplicitRouteUnix`
- [x] §6.9 `TestRouteUnixDial` (explicit, abstract, missing path retry, error text, implicit retry budget, backoff, reload removal, self skip, peer close, cluster authorization good/bad/missing)
- [x] §6.11 `TestRouteUnixReload` add/remove rows (route add, no-op, advertise add/change/remove, rejections)
- [x] §6.12 `TestRouteUnixTLS` (hostname from TCP route, insecure, no name, ip-only)
- [x] §7.1 `TestRouteUnixThreeServerMesh`, `TestRouteUnixMeshTopologies` (full mesh, ring without/with advertise, star, mixed tcp/unix)
- [x] §7.1 `TestNoRaceRouteUnixThreeServerMeshTraffic` in `server/norace_2_test.go` (100k x 128B fan-out to two peers, all routes over unix)
- [x] Full existing route suite passes with `-race`: `go test -race -run 'TestRoute|TestUnix|TestCluster' ./server/` (skipping the pre-existing flaky `TestRouteSlowConsumerRecover`, see evidence log)

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

Phase 4 (2026-09-22), no-race fan-out over a three-server unix mesh:

```
$ go test -count=1 -p=1 ./server -run TestNoRaceRouteUnix -tags=skip_no_race_1_tests -v
    norace_2_test.go:4001: 100000 messages of 128 bytes fanned out to 2 servers over unix routes in 491.517411ms (203452 msgs/s)
--- PASS: TestNoRaceRouteUnixThreeServerMeshTraffic (0.58s)
```

Pre-existing on `main` (`edb1b17a`), not caused by this branch: `TestRouteSlowConsumerRecover`
fails on this machine (`Expected Slow Consumer routes`, bandwidth-shaping proxy timing).
`TestClusteredInterestConsumerFilterEdit` (JetStream over TCP routes, hard message-count
assertions with no retry) failed once in a full `-race` run of `TestRoute|TestUnix|TestCluster`
and passed 8/8 isolated reruns and a second full run; timing under load, unrelated to transport.

## Session log

| Date | Work done | Next |
|---|---|---|
| 2026-09-22 | Codebase survey; design doc written; this status file created. | Open issue (phase 0); start phase 1 helpers and §6.1 table. |
| 2026-09-22 | Plan validated against code; design doc corrected (json tag, `setBaselineOptions` Host guard, 12 predicate sites, reject `%`, TLS error wording). Phase 1 helpers, per-OS files and tests written; `-race` green; windows vet, wasm/darwin/freebsd builds ok. Committed. | Phase 2: `UnixSocket` option, `listenEnabled()`, parse/validate/reload, predicate sites, tests. |
| 2026-09-22 | Phase 2 done: `UnixSocket` field, `listenEnabled()` at all 12 sites, parse-time and `validateCluster` transport rules, `routesFromStr` error variant for `-routes`, `overrideCluster` unix branch, reload rejection. Fixed phase-1 length rule (pathname limit is `sun_path`-1, abstract is `sun_path`, matching Go's `SockaddrUnix`). Found the conf lexer accepts unquoted `unix:///...`. `-race` green on config/options/reload sets; `./test` route suite green. Committed. | Phase 3: `listenRouteUnix`, `startRouteAcceptLoop` transport switch, accessors, `unixRoutesToSelf`, real-socket tests. |
| 2026-09-22 | Phase 3 done: `listenRouteUnix`/`removeStaleUnixSocket` (Lstat, not-a-socket, probe, `isConnRefused`, remove + warn + counter), accept loop transport switch, `udsStats` embedded, `unixRoutesToSelf`, `ClusterUnixAddr`/`ClusterListenAddr`, `formatURL` unix branch with `urlUnixAddr` inverse per OS, `initClient` host from `*net.UnixAddr`. Real-socket tables green under `-race`; route/client/ports regressions and `./test` suites green. Committed. | Phase 4: `setRouteInfoHostPortAndIP`, `connectToRoute` unix dial, `processRouteInfo`/`processImplicitRoute`/`hasThisRouteConfigured`, TLS name fallback, three-server mesh tests. |
| 2026-09-22 | Phase 4 done: `setRouteInfoHostPortAndIP` transport cases, `connectToRoute` unix dial with self skip and counters, `processRouteInfo`/`processImplicitRoute`/`hasThisRouteConfigured` unix `info.IP` handling, `saveRouteTLSName` skip, `doTLSHandshake` name fallback and `errRouteTLSUnixNoName`, `sendRouteConnect` cluster-auth fallback for explicit unix routes (design gap found by test; §3.1 updated). Dial, reload, TLS, mesh and topology tables plus 100k-message no-race fan-out green; `-race` on route/unix/cluster sets and `./test` suites green; windows vet, wasm/darwin/freebsd builds ok. Committed. | Phase 5: `route.transport`, Varz `UnixSocket`/`UnixSocketStats`, Routez `Transport`/`UnixSocket`, STATSZ `RouteStat.Transport`, `TestRouteUnixMonitoring`, golden TCP `/varz` fragment. |
