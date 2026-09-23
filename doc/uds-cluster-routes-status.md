# Status: Cluster routes over Unix domain sockets

Tracks implementation of [uds-cluster-routes.md](uds-cluster-routes.md). Update this file
at the end of every working session. Section numbers (§) refer to the design doc.

Legend: `[ ]` not started · `[~]` in progress · `[x]` done · `[!]` blocked

Last updated: 2026-09-22

## Summary

| Phase | Scope | State | Notes |
|---|---|---|---|
| 0 | GitHub issue | `[ ]` | Not opened yet. Dave writes it by hand (issue template checkbox forbids AI-written or pasted text). Source material: "Upstream briefing" below and design §1.1/§1.2. |
| 1 | Address parsing, canonical URL helpers, per-OS files | `[x]` | Committed on `uds-cluster-routes`. |
| 2 | Options, flags, validation, reload rejection | `[x]` | Committed. |
| 3 | Listener, stale socket, self-route map, accessors | `[x]` | Committed. |
| 4 | Dial, gossip, INFO, TLS name fallback | `[x]` | Committed. `sendRouteConnect` cluster-auth fallback added for explicit unix routes. |
| 5 | Monitoring fields and counters | `[x]` | Committed. `dialed` counts successes only; `cluster.urls` renders unix routes. |
| 6 | Multi-process configs, script, docs | `[x]` | Committed. Script passes in full-mesh and `UDS_PAIRS=1` (socat) modes. Real-proxy run: runbook written below, run pending (Dave, from the `uds-rdma-proxy` repo using this branch). |
| 7 | PR: draft, squash, sign-off, evidence | `[ ]` | Briefing below. Commit shape recommendation: two commits by area, docs on a separate fork branch. |

Branch: `uds-cluster-routes` (from `main` at `edb1b17a`)

## Phase 0: issue

- [ ] Issue opened (Proposal template), number: `#____`
- [ ] Design doc and fork branch linked from the issue
- [ ] Prior art linked: `#7800`, `#7677`, `#1216`, `containerd/containerd#13569`
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
- [x] `unixRouteURLFromString`: gossiped `IP` may be the `url.URL.String()` form with percent-encoded non-ASCII path bytes; `processRouteInfo`, `processImplicitRoute` and `hasThisRouteConfigured` accept both forms (Dave, post-review)
- [x] `updateUnixRoutesToSelf`: self-route skip set rebuilt on startup and on cluster reload so an `advertise` change drops the stale entry (Dave, post-review); `connectToRoute` now does the self lookup under the server lock because reload rewrites the map
- [x] Mixed old/new cluster behaviour measured with `main` binaries (design §4.4.1)

Tests:

- [x] §6.8 `TestRouteInfoUnix`, `TestHasThisRouteConfiguredUnix`, `TestProcessImplicitRouteUnix` (plus percent-encoded canonical URL rows), `TestUnixRouteURLFromString`
- [x] §6.9 `TestRouteUnixDial` (explicit, abstract, missing path retry, error text, implicit retry budget, backoff, reload removal, self skip, peer close, cluster authorization good/bad/missing)
- [x] §6.11 `TestRouteUnixReload` add/remove rows (route add, no-op, advertise add/change/remove, rejections)
- [x] §6.12 `TestRouteUnixTLS` (hostname from TCP route, insecure, no name, ip-only)
- [x] §7.1 `TestRouteUnixThreeServerMesh`, `TestRouteUnixMeshTopologies` (full mesh, ring without/with advertise, star, mixed tcp/unix)
- [x] §7.1 `TestNoRaceRouteUnixThreeServerMeshTraffic` in `server/norace_2_test.go` (100k x 128B fan-out to two peers, all routes over unix)
- [x] Full existing route suite passes with `-race`: `go test -race -run 'TestRoute|TestUnix|TestCluster' ./server/` (skipping the pre-existing flaky `TestRouteSlowConsumerRecover`, see evidence log)

## Phase 5: monitoring (§3.3)

Files: `server/server.go`, `server/monitor.go`, `server/events.go`

- [x] `udsStats` struct embedded in `Server` at the aligned head, `atomic.Uint64` fields (phase 3)
- [x] Increments at accept, dial success, dial failure, stale removal (`dialed` counted attempts until phase 5; now successes only, per §3.3)
- [x] `RouteUnixSocketStats` type; `ClusterOptsVarz.UnixSocket` and `.UnixSocketStats` (both `omitempty`; stats nil unless unix listener or non-zero counter)
- [x] `updateVarzRuntimeFields` copies counters via `routeUnixSocketStats()`; `Active` via `forEachRoute` reading `route.transport`
- [x] `RouteInfo.Transport` and `.UnixSocket`; `Routez` switch on `RemoteAddr().(type)` (nil-safe when the connection is gone); `route.transport` captured in `createRoute` via `routeTransport(conn)`
- [x] `RouteStat.Transport` in STATSZ
- [x] Inline `//` doc comment on every new field (monitor.go convention); `TestMonitorVarz` positional `ClusterOptsVarz` guard literal extended

Tests:

- [x] §6.10 `TestRouteUnixMonitoring` (varz listener/counters/dial errors, routez unix and tcp regression, STATSZ both transports, ports file, counters after dialer restart, stale removal, transport kept after close) over HTTP and API modes; `TestRouteTransport` table
- [x] Golden `/varz` JSON for a TCP-only server unchanged (`TestVarzClusterTCPGolden`)

## Phase 6: multi-process integration (§7.2, §7.3)

Files: `test/configs/uds/srv_{a,b,c}.conf`, `scripts/uds-cluster-smoke.sh`, docs

- [x] Three config files (`srv_{a,b,c}.conf`), plus `srv_{a,b,c}_pairs.conf` for the ring
- [x] Script: build, config-check, start, wait for two peers per server and every route `transport == "unix"` (`num_routes` is `2 * pool_size`, 6 by default)
- [x] Script: `ss -xlp` / `ss -xp` evidence capture (saved under `$OUT`)
- [x] Script: pub/sub and `nats bench` traffic
- [x] Script: SIGKILL B, restart, `Removed stale unix socket` log line, `stale_removed: 1` in `/varz`, mesh re-forms
- [x] Script: SIGTERM all, assert no `{a,b,c}.sock` files remain
- [x] `UDS_PAIRS=1` mode with `socat` stand-in for the proxy; `UDS_PROXY=1` skips socat for the real proxy
- [x] `UDS_PROXY=1` skips the socat far-end check (kernel proxy has no process) and saves `UDS_PROXY_STATS` output before/after the bench
- [ ] Run against the real `uds-over-rdma-proxy`: single host over soft-RoCE, then hp1/hp2/hp3 (runbook below); paste evidence into the evidence log
- [x] `test/configs/uds/README.md` documents `unix://` listen and routes; `nats.docs` update recorded as a follow-up (design §11)

## Phase 7: PR

- [ ] `upstream` remote added, branch rebased on latest `nats-io/nats-server` `main`
- [ ] Commits re-authored by area (see "Commit shape" below), each `Signed-off-by: David Seddon <dave.seddon.ca@gmail.com>`
- [ ] Docs moved to a separate fork branch (`uds-cluster-routes-docs`) so the PR branch carries no `doc/` files
- [ ] `golangci-lint run` clean
- [ ] Branches pushed to `randomizedcoder/nats-server`
- [ ] Draft PR opened, number: `#____`
- [ ] PR description written by hand (template notice deleted) with `ss -xp`, `/routez`, bench and `urp stats` evidence, `Resolves #NNN`
- [ ] CI green: lint, linux amd64/386, windows-2022/2025, race and no-race shards
- [ ] Marked ready for review
- [ ] Follow-up issue filed on `prometheus-nats-exporter` for the new fields
- [ ] Follow-up PR or issue on `nats-io/nats.docs` for the `unix://` syntax and monitoring fields

## Upstream briefing (source material; Dave writes the prose)

The Proposal issue template ends with a required checkbox: "I am a human being writing in my
own words and not an AI agent. I will not use an AI agent to communicate on my behalf in this
issue, either directly or via copy-paste." `CONTRIBUTING.md` says the same for PRs ("do not
meat-proxy"; the PR template adds "AI agents should not open pull requests directly"). So
everything in this section is facts and pointers to rewrite, never text to paste.

### Issue (template: Proposal, labels it `proposal`)

**Proposed change** box, facts to cover:

- Cluster `listen`, `routes` and `advertise` accept `unix:///abs/path` and `unix://@name`
  (Linux abstract) in the config file and the `-cluster`, `-routes`, `-cluster_advertise` flags.
- Everything above the `net.Conn` is untouched: auth, INFO exchange, compression, pooling,
  per-account routes, JetStream. TCP-only servers produce byte-identical `/varz` (golden test).
- Mixed clusters work: a server may listen on a socket and dial some peers over TCP.
- No new dependency; per-OS files only for path normalisation and `ECONNREFUSED`.
- Monitoring additions are `omitempty`: `/varz` `cluster.unix_socket` and
  `cluster.unix_socket_stats{accepted,dialed,dial_errors,stale_removed,active}`, `/routez`
  `transport` and `unix_socket`, STATSZ `routes[].transport`.
- Config example (from `test/configs/uds/srv_a.conf`):

  ```hcl
  cluster {
    name: uds
    listen: "unix:///run/nats/a.sock"
    routes = [ "unix:///run/nats/b.sock", "unix:///run/nats/c.sock" ]
  }
  ```

- Limits, stated up front (design §1.1 last paragraph): routes only; no advertise by default so
  unix-only servers list every peer; mixed old/new degrades to explicit routes with one error
  line per gossiped unix-only peer on old servers (design §4.4.1 table); TLS over a unix route
  needs `insecure` or a TCP hostname; Windows compile and unit coverage only until CI.

**Use case** box: design §1.1 items 1-6 in Dave's words, with the RDMA tunnel as the worked
example and the `urp stats`/bench numbers from the evidence log once the real-proxy run is done.
Prior art to link with one line each (design §1.2): `#7800` (client-listener PR, stale-closed,
no issue first), `#7677` (Derek: compelling use cases; Neil: scope worry, hence transport-only
here), `#1216` (monitoring over a socket file, same security motivation),
`containerd/containerd#13569` (the grammar).

**Contribution** box: implementation exists on `randomizedcoder/nats-server` branch
`uds-cluster-routes` (link the branch and the design doc on `uds-cluster-routes-docs`);
tested under `-race` plus a three-process smoke script; draft PR to follow once maintainers
react; two follow-ups planned (`prometheus-nats-exporter`, `nats.docs`).

### PR description (draft PR, hand-written, `Resolves #NNN`)

- What changed, by area, with the test that covers it:
  - parsing and options (`server/uds*.go`, `opts.go`): `TestParseUnixAddr`,
    `TestUnixRouteURLFromString`, `TestClusterOptsUnixSocketConfig`, `TestClusterUnixFlags`,
    `TestConfigCheck` rows;
  - listener (`route.go` accept loop, `uds.go` stale-socket handling):
    `TestListenRouteUnixStaleSocket`, `TestServerUnixListenerAccessors`;
  - dial, gossip, TLS (`route.go`, `client.go`): `TestRouteInfoUnix`,
    `TestHasThisRouteConfiguredUnix`, `TestProcessImplicitRouteUnix`, `TestRouteUnixDial`,
    `TestRouteUnixReload`, `TestRouteUnixTLS`, `TestRouteUnixThreeServerMesh`,
    `TestRouteUnixMeshTopologies`, `TestNoRaceRouteUnixThreeServerMeshTraffic`;
  - monitoring (`monitor.go`, `events.go`): `TestRouteUnixMonitoring` plus the TCP golden test;
  - smoke tooling: `test/configs/uds/`, `scripts/uds-cluster-smoke.sh`.
- Evidence to paste from the evidence log: `ss -xlp`/`ss -xp` lines, a `/routez` excerpt with
  `"transport":"unix"`, `nats bench` figures for the socat ring and the real proxy, `urp stats`
  deltas, the mixed-version table from design §4.4.1 as a "Compatibility" paragraph.
- Platform status: Linux measured; macOS/BSD via Go's `net` (untested here); Windows compile
  and unit tests only, runtime relies on the windows CI runners.
- Questions for maintainers, asked explicitly rather than decided: TLS-over-unix fallback
  policy (§4.6); whether the monitoring fields are the shape they want before the exporter
  follows; whether a route `Proto` gate is wanted for mixed clusters; where documentation
  should live (`nats.docs` PR offered).
- CI expectation: the local `srv_pkg_non_js_tests` shard aborts on pre-existing fixed-port
  collisions on `main` too (evidence log), so GitHub Actions is the clean signal.

### Commit shape (recommendation)

Upstream merges with merge commits, so PR commits survive as review units. Recommendation:
re-author the seven phase commits into two, by area, and keep `doc/` off the PR branch
(upstream `doc/` holds only a `README.md`; the reviewer already flagged the design docs).

1. `git branch uds-cluster-routes-phases` (keep the seven-commit history).
2. `git remote add upstream https://github.com/nats-io/nats-server.git && git fetch upstream`,
   `git rebase upstream/main`, rerun the UDS slice.
3. `git reset --soft upstream/main`, then two commits with typed trailers
   (`git config user.name` is `randomizedcoder`, so `-s` would sign with the wrong name):
   `[ADDED] Cluster: routes over Unix domain sockets` (everything under `server/`) and
   `[ADDED] Cluster: unix socket route smoke test configs and script` (`test/configs/uds/`
   with `git add -f`, `scripts/uds-cluster-smoke.sh`).
4. `git checkout -b uds-cluster-routes-docs`, one commit with `doc/`; rebase it onto the PR
   branch whenever that moves. `git diff uds-cluster-routes-docs uds-cluster-routes-phases`
   must be empty.
5. Push both branches to `origin`; the issue links the docs branch, the PR uses the code branch.

## Real-proxy test runbook

Rough procedure for proving the routes work through `uds-over-rdma-proxy` (`urp`, a kernel
module plus the `urp` CLI, repo `~/Downloads/uds-rdma-proxy`). Two stages; the first needs only
this machine, the second the hp1/hp2/hp3 RoCEv2 testbed. Both reuse this branch's binary and
`test/configs/uds/srv_{a,b,c}_pairs.conf` (ring: A dials `ab.sock`, B dials `bc.sock`, C dials
`ca.sock`; the proxy forwards `ab`→`b.sock`, `bc`→`c.sock`, `ca`→`a.sock`).

Facts about `urp` that the procedure depends on (from its README and design docs 23, 33):

- One `urp add` per tunnel end. Acceptor: `urp add <name> --connect-path <far>.sock --bind
  <rdma ip>:<port>`. Initiator: `urp add <name> --listen-path <near>.sock --peer <rdma
  ip>:<port>`. Distinct port per edge.
- The acceptor connects to `--connect-path` lazily, when the initiator accepts its first
  stream (design 33 phase 2), so endpoint order is not load-bearing; adding acceptors before
  initiators keeps logs clean.
- Counters: `urp stats [<name>]`, `urp show <name>`, `/proc/urp/<name>/stats`.
- The module is `init_net`-only; network namespaces do not isolate endpoints. Root required.
- Single-host loopback uses soft-RoCE: `modprobe rdma_rxe; rdma link add rxe0 type rxe netdev
  <nic>` and the NIC's IPv4 as the RDMA address (pattern: `nix/test-redpanda-uds.nix`).

### Stage 1: single host over soft-RoCE

Run as root on this machine. Socket directory must be short (`/tmp/nats-uds`).

1. Build and load: `nix build .#urp-ko` (or the flake's module package for `uname -r`),
   `insmod result/lib/modules/$(uname -r)/urp.ko`; set up `rdma_rxe` on the NIC that has an
   IPv4 address; note `RXE_IP`.
2. From this repo: `go build -o /tmp/nats-uds-bin/nats-server .` and start the three servers
   by hand with `srv_{a,b,c}_pairs.conf`, or let the smoke script do it (step 4) and create the
   endpoints while it waits (`TIMEOUT=60`).
3. Endpoints (three edges, ports 4791-4793):

   ```
   urp add nats_ab_acc --connect-path /tmp/nats-uds/b.sock --bind $RXE_IP:4791
   urp add nats_bc_acc --connect-path /tmp/nats-uds/c.sock --bind $RXE_IP:4792
   urp add nats_ca_acc --connect-path /tmp/nats-uds/a.sock --bind $RXE_IP:4793
   urp add nats_ab     --listen-path  /tmp/nats-uds/ab.sock --peer $RXE_IP:4791
   urp add nats_bc     --listen-path  /tmp/nats-uds/bc.sock --peer $RXE_IP:4792
   urp add nats_ca     --listen-path  /tmp/nats-uds/ca.sock --peer $RXE_IP:4793
   ```

4. `UDS_PAIRS=1 UDS_PROXY=1 UDS_PROXY_STATS='urp stats' TIMEOUT=60
   scripts/uds-cluster-smoke.sh`. Expected: mesh forms (two distinct unix peers per server),
   pub/sub A←C, bench, `proxy-stats-before/after.txt` show the bench bytes on all three edges,
   SIGKILL/restart of B re-forms the ring (this exercises the acceptor reconnecting to a fresh
   `b.sock`; watch for it), SIGTERM leaves no `a/b/c.sock`.
5. Negative attribution (proves the bytes went through the tunnel): with the cluster up,
   `urp remove nats_ab_acc`; within the route reconnect interval A's `/routez` loses `uds-b`
   while A↔C stays; `urp add` it back and the ring re-forms.
6. Teardown: `urp remove` all six, `rmmod urp`, `rdma link delete rxe0`. Paste `ss -xlp`,
   `ss -xp`, `/routez` from one server, bench output and both `urp stats` captures into the
   evidence log.

Good candidate for a `nix/test-nats-uds.nix` runner in the proxy repo, copied from
`test-redpanda-uds.nix`, with this branch as a `flake = false` input built by `buildGoModule`
and `pkgs.natscli` for the bench.

### Stage 2: hp1/hp2/hp3 over real RoCEv2

One `nats-server` per host, ring through three RDMA edges; mirrors the proxy repo's
`urp-mesh-matrix.nix` (ssh as root, `nix copy` the binaries, edge subnets from its header:
1-2 `10.10.12.0/29`, 1-3 `10.10.13.0/29`, 2-3 `10.10.23.0/29`).

1. Per host, a config like `srv_a_pairs.conf` with `/tmp/nats-uds` replaced by
   `/run/nats-uds`, `server_name` `hpN`, `listen 127.0.0.1:4222`, `http 127.0.0.1:8222`,
   cluster `listen unix:///run/nats-uds/self.sock`, one route
   `unix:///run/nats-uds/next.sock`. Start with `systemd-run --unit nats-uds nats-server -c ...`.
2. Per edge hp1→hp2, hp2→hp3, hp3→hp1: on the destination host
   `urp add nats_acc --connect-path /run/nats-uds/self.sock --bind <dest edge ip>:4791`; on
   the source host `urp add nats_init --listen-path /run/nats-uds/next.sock --peer <dest edge
   ip>:4791`.
3. Checks over ssh: `curl -s 127.0.0.1:8222/routez` on every host shows two distinct
   `remote_name` values and `"transport":"unix"` on every route; `ss -xp` per host shows
   `nats-server` on `self.sock` and nothing on the far end of `next.sock`; `nats bench sub` on
   hp3 and `nats bench pub` on hp1 (crosses two RDMA edges in the ring); `urp stats` deltas on
   all six endpoints; kill and restart one server; teardown with `urp remove`, `systemctl stop
   nats-uds`, and no leftover socket files.
4. Record: throughput, `ss -xp` lines (no TCP route sockets anywhere), `/routez` excerpt,
   `urp stats`. This is the headline evidence for the PR's "why".

Things to watch rather than assume: acceptor reconnect after a server SIGKILL (design 41
stream replay vs. a fresh UDS connect); route pool size 3 means three UDS connections per edge,
each a tunnel stream; `urp` design 16 (no `SCM_RIGHTS`) does not affect NATS.

## Open questions

Record decisions here with the date so they are not re-litigated.

| Date | Question | Decision |
|---|---|---|
| 2026-09-22 | Credentials inside `unix://` URLs? | No. `@` collides with abstract marker; use cluster `authorization {}` (§3.1). Revisit if maintainers ask. |
| 2026-09-22 | Advertise listen path by default for UDS? | No. Paths are namespace-local; explicit routes form the mesh (§4.4). |
| 2026-09-22 | Route `Proto` bump? | Not proposed. Mixed old/new behaviour measured (§4.4.1): degrades to explicit routes with one error line per gossiped unix-only peer on old servers. Offer a `Proto` gate only if maintainers ask. |
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

Phase 5 (2026-09-22), three `nats-server` processes, full explicit mesh over unix sockets
(default `pool_size`, so 3 pooled + 1 system-account route per peer), started
simultaneously so some early dials fail and are retried:

```
$ nats-server -t -c a.conf
nats-server: configuration file a.conf is valid (sha256:87237a16...)
$ curl -s http://127.0.0.1:$PORT_A/routez | jq '{num_routes, t: [.routes[].transport] | unique}'
{"num_routes": 8, "t": ["unix"]}
$ curl -s http://127.0.0.1:$PORT_A/varz | jq .cluster
{"name":"uds","auth_timeout":2,"urls":["unix:///tmp/nuds/b.sock","unix:///tmp/nuds/c.sock"],
 "tls_timeout":2,"pool_size":3,"unix_socket":"/tmp/nuds/a.sock",
 "unix_socket_stats":{"accepted":4,"dialed":8,"dial_errors":0,"stale_removed":0,"active":8}}
$ ss -xlp | grep nuds
u_str LISTEN 0 4096 /tmp/nuds/b.sock ... users:(("nats-server",pid=276649,fd=8))
u_str LISTEN 0 4096 /tmp/nuds/c.sock ... users:(("nats-server",pid=276650,fd=8))
u_str LISTEN 0 4096 /tmp/nuds/a.sock ... users:(("nats-server",pid=276648,fd=8))
$ ss -xp | grep -c nuds        # established route connections, both ends
12
$ pkill -TERM nats-server; ls /tmp/nuds | wc -l
0
```

The first attempt used the session scratchpad directory and was rejected by `-t` with
`unix socket address "unix:///tmp/claude-.../a.sock" is too long: 114 bytes, max is 107`,
which is the intended length rule. The smoke also exposed that `/varz` `cluster.urls`
rendered unix routes as empty strings (`urlsToStrings` used `u.Host`); fixed in phase 5.

Pre-existing on `main` (`edb1b17a`), not caused by this branch: `TestRouteSlowConsumerRecover`
fails on this machine (`Expected Slow Consumer routes`, bandwidth-shaping proxy timing).
`TestClusteredInterestConsumerFilterEdit` (JetStream over TCP routes, hard message-count
assertions with no retry) failed once in a full `-race` run of `TestRoute|TestUnix|TestCluster`
and passed 8/8 isolated reruns and a second full run; timing under load, unrelated to transport.
`TestRoutePoolConnectRace` and `TestRoutePerAccountConnectRace` (TCP route pools) failed once
when the `-race` suite ran concurrently with a second `-race` suite, golangci-lint and the
no-race fan-out; 3/3 isolated reruns and a solo rerun of the full suite passed.

Phase 5 (2026-09-22), `golangci-lint run --config=.golangci.yml ./server/...`: clean after
fixing one `misspell` finding in a test comment.

Phase 6 (2026-09-22), `scripts/uds-cluster-smoke.sh`, full mesh, five consecutive runs
`passed: 15 failed: 0`. Key lines from one run:

```
PASS: mesh formed: every server sees 2 peers, all routes transport unix
u_str LISTEN 0 4096 /tmp/nats-uds/a.sock ... users:(("nats-server",pid=375342,fd=8))
u_str LISTEN 0 4096 /tmp/nats-uds/c.sock ... users:(("nats-server",pid=375363,fd=8))
u_str LISTEN 0 4096 /tmp/nats-uds/b.sock ... users:(("nats-server",pid=375353,fd=8))
PASS: ss -xlp shows 3 nats-server unix listeners      (ss -xp: 9 ESTAB, 3 pairs x pool 3)
routez a (num_routes peers unix): 6 2 6               (same for b and c)
PASS: message published on C was delivered to the subscriber on A
NATS Core NATS subscriber stats: 507,220 msgs/sec ~ 62 MiB/sec   (100k x 128 B, A -> B)
PASS: b.sock left behind after SIGKILL
PASS: restarted b logged stale socket removal: Removed stale unix socket "/tmp/nats-uds/b.sock"
    "unix_socket_stats": { "accepted": 0, "dialed": 6, "dial_errors": 0, "stale_removed": 1, "active": 6 }
PASS: no server socket files remain in /tmp/nats-uds after SIGTERM
```

Phase 6 (2026-09-22), `UDS_PAIRS=1` ring through three `socat` pairs: `passed: 16 failed: 0`.
`ss -xlp` shows `ab/bc/ca.sock` owned by `socat` and `a/b/c.sock` by `nats-server`; `ss -xp`
shows every established route socket with `socat` on the far end, never a peer server.
Bench through the proxy: `466,308 msgs/sec ~ 57 MiB/sec`. B's SIGKILL/restart re-forms the ring.

Script bug found while writing it: the evidence helper teed into the same file `cat` was
reading, truncating it first, so `/varz` checks failed at random. Fixed by displaying saved
files without re-teeing. Not a server bug.

Post-review (2026-09-22), mixed old/new cluster. Binaries: `main` (`edb1b17a`, built with
`-buildvcs=false` from a worktree) as `old-1`/`old-2` meshed over TCP (`connect_retries: 2`),
branch binary as `new-uds` with `listen: "unix:///tmp/nats-mixed/n.sock"` and
`routes = [ nats-route://127.0.0.1:16322 ]` (old-1 only), started after the old pair meshed.

```
# newcomer without advertise: old-2 (debug logging) is gossiped nats-route://127.0.0.1:0/
[DBG] Trying to connect to route on 127.0.0.1:0 (127.0.0.1:0)
[ERR] Error trying to connect to route (attempt 1): dial tcp 127.0.0.1:0: connect: connection refused
[DBG] Error trying to connect to route (attempt 2): dial tcp 127.0.0.1:0: connect: connection refused
[DBG] Error trying to connect to route (attempt 3): dial tcp 127.0.0.1:0: connect: connection refused
# newcomer with advertise: "unix:///tmp/nats-mixed/n.sock": old-2 is gossiped the unix URL
[ERR] Error trying to connect to route (attempt 1): missing port in address
# routez peers afterwards (pool_size 3), both cases
old-1:   3 x new-uds, 3 x old-2
old-2:   3 x old-1
new-uds: 3 x old-1 (transport tcp)
```

When the newcomer started *before* the old pair finished forming its pool, old-1 forwarded
old-2's INFO to the newcomer as a side effect and the newcomer dialed old-2 over TCP, giving a
full mesh. That is ordering luck, not a guarantee; the steady-state rule is the table in
design §4.4.1. An all-`main` control with the same ordering behaved identically apart from the
error text, confirming the "newcomer learns nothing from the seed" gossip direction is
pre-existing.

Post-review (2026-09-22), the two failures Dave saw in a full `go test ./server`:
`TestServerEventsHealthZClustered_NoReplicas` and `TestGatewayImplicitReconnect` pass 3/3 on
the branch and 3/3 on a clean `main` worktree. The CI `srv_pkg_non_js_tests` shard command
(`-race -p=1 -failfast`, same regex and tags as `scripts/runTestsOnTravis.sh`) was run on the
branch; first attempt stopped at 552 passes on
`TestLeafNodeWithWeightedDQRequestsToSuperClusterWithSeparateAccounts` with
`route(listen tcp 127.0.0.1:24024: bind: address already in use)` while the mixed-version
smoke above was running alongside it; that test passes 2/2 in isolation. Second, solo attempt
stopped at 653 passes on `TestSubszOperatorMode` with `server(listen tcp 127.0.0.1:5500: bind:
address already in use)`; that test and its predecessor at `monitor_test.go:4665` both hard-code
port 5500, and it passes 3/3 in isolation. Neither failure touches unix routes. Third run
without `-failfast`: the package binary still aborts on the first panic; it died on
`TestGatewayIgnoreSelfReference` (`gateway(listen tcp 127.0.0.1:5222: bind: address already in
use)`, its predecessor `TestGatewayBasic` hard-codes gateway port 5222) after 468 passes, plus
`TestPSEmulationCPU` in `server/pse` (`CPUs did not match close enough: 0.000000 vs 50.000000`).
Both pass or fail identically off-branch: the gateway test passes 3/3 alone, and
`TestPSEmulationCPU` fails 3/3 on clean `main` too (procps `ps` output on NixOS; the branch
does not touch `server/pse`).

Control: the identical shard on a clean `main` worktree (`edb1b17a`) aborted the same way,
673 passes then `TestLeafNodeWithWeightedDQRequestsToSuperClusterWithSeparateAccounts` with
`route(listen tcp 127.0.0.1:22280: bind: address already in use)` and the same
`TestPSEmulationCPU` failure. Conclusion: this shard cannot complete on this machine on any
branch because of hard-coded test ports and the `ps` emulation check; nothing in the four
attempts points at unix routes. Across the four runs every test that got to run passed except
those fixed-port panics and `TestPSEmulationCPU`. The clean full-CI signal has to come from
GitHub Actions on the draft PR.

## Session log

| Date | Work done | Next |
|---|---|---|
| 2026-09-22 | Codebase survey; design doc written; this status file created. | Open issue (phase 0); start phase 1 helpers and §6.1 table. |
| 2026-09-22 | Plan validated against code; design doc corrected (json tag, `setBaselineOptions` Host guard, 12 predicate sites, reject `%`, TLS error wording). Phase 1 helpers, per-OS files and tests written; `-race` green; windows vet, wasm/darwin/freebsd builds ok. Committed. | Phase 2: `UnixSocket` option, `listenEnabled()`, parse/validate/reload, predicate sites, tests. |
| 2026-09-22 | Phase 2 done: `UnixSocket` field, `listenEnabled()` at all 12 sites, parse-time and `validateCluster` transport rules, `routesFromStr` error variant for `-routes`, `overrideCluster` unix branch, reload rejection. Fixed phase-1 length rule (pathname limit is `sun_path`-1, abstract is `sun_path`, matching Go's `SockaddrUnix`). Found the conf lexer accepts unquoted `unix:///...`. `-race` green on config/options/reload sets; `./test` route suite green. Committed. | Phase 3: `listenRouteUnix`, `startRouteAcceptLoop` transport switch, accessors, `unixRoutesToSelf`, real-socket tests. |
| 2026-09-22 | Phase 3 done: `listenRouteUnix`/`removeStaleUnixSocket` (Lstat, not-a-socket, probe, `isConnRefused`, remove + warn + counter), accept loop transport switch, `udsStats` embedded, `unixRoutesToSelf`, `ClusterUnixAddr`/`ClusterListenAddr`, `formatURL` unix branch with `urlUnixAddr` inverse per OS, `initClient` host from `*net.UnixAddr`. Real-socket tables green under `-race`; route/client/ports regressions and `./test` suites green. Committed. | Phase 4: `setRouteInfoHostPortAndIP`, `connectToRoute` unix dial, `processRouteInfo`/`processImplicitRoute`/`hasThisRouteConfigured`, TLS name fallback, three-server mesh tests. |
| 2026-09-22 | Phase 4 done: `setRouteInfoHostPortAndIP` transport cases, `connectToRoute` unix dial with self skip and counters, `processRouteInfo`/`processImplicitRoute`/`hasThisRouteConfigured` unix `info.IP` handling, `saveRouteTLSName` skip, `doTLSHandshake` name fallback and `errRouteTLSUnixNoName`, `sendRouteConnect` cluster-auth fallback for explicit unix routes (design gap found by test; §3.1 updated). Dial, reload, TLS, mesh and topology tables plus 100k-message no-race fan-out green; `-race` on route/unix/cluster sets and `./test` suites green; windows vet, wasm/darwin/freebsd builds ok. Committed. | Phase 5: `route.transport`, Varz `UnixSocket`/`UnixSocketStats`, Routez `Transport`/`UnixSocket`, STATSZ `RouteStat.Transport`, `TestRouteUnixMonitoring`, golden TCP `/varz` fragment. |
| 2026-09-22 | Phase 5 done: `route.transport` captured at creation, `RouteUnixSocketStats` and `ClusterOptsVarz.UnixSocket`/`UnixSocketStats` (nil for TCP-only servers, golden test), `RouteInfo.Transport`/`UnixSocket` with nil-safe `Routez` address switch, `RouteStat.Transport` in STATSZ. Fixed `dialed` to count successes only (design §3.3). `/varz` `cluster.urls` now renders unix routes (found by the three-process smoke). Monitoring tables over HTTP and API green under `-race`; monitor/varz/routez/events regressions, `./test` suites, cross builds and golangci-lint checked. Committed. Stopped for review. | Review phases 1-5; then phase 6 (multi-process configs, `scripts/uds-cluster-smoke.sh`, `ss -xp` evidence) and phase 7 (issue, draft PR). |
| 2026-09-22 | Phase 6 done: `test/configs/uds/srv_{a,b,c}.conf` and `srv_{a,b,c}_pairs.conf`, `scripts/uds-cluster-smoke.sh` (build, `-t`, start, wait for two unix peers per server, `ss -xlp`/`ss -xp` evidence, pub/sub, `nats bench`, SIGKILL/restart with stale-socket check, SIGTERM with no leftover sockets, PASS/FAIL summary), `UDS_PAIRS=1` socat ring and `UDS_PROXY=1` hook for the real proxy, `test/configs/uds/README.md`. Design §7.2/§7.3 updated to the shipped script (peer-count criterion, real log text). Five full-mesh runs and one ring run green. Committed. | Dave: run `UDS_PAIRS=1 UDS_PROXY=1 scripts/uds-cluster-smoke.sh` against `uds-over-rdma-proxy` and paste evidence; write the GitHub issue; then phase 7 (rebase, draft PR with hand-written description). |
| 2026-09-22 | Phase 7 prep: design §1.1 (six motivations, stated limits) and §1.2 (prior art: PR `#7800` stale-closed with no maintainer comment, discussion `#7677` scope worry, issue `#1216`, containerd `#13569`); status doc "Upstream briefing" (issue and PR fact sheets, commit-shape recommendation: two commits by area, docs on a separate fork branch) and "Real-proxy test runbook" (single-host soft-RoCE, then hp1/hp2/hp3); smoke script `UDS_PROXY=1` no longer expects a `socat` process and saves `UDS_PROXY_STATS` output before/after the bench; README section on running against a real proxy. | Dave: write the issue from the briefing; run stage 1 and 2 of the runbook from the `uds-rdma-proxy` repo using this branch and paste evidence; then re-author commits, push, draft PR. |
| 2026-09-22 | Post-review: Dave added `unixRouteURLFromString` (percent-encoded gossip form) and `updateUnixRoutesToSelf` (reload-safe self-route set) with regression rows; `connectToRoute` self lookup moved under the server lock (reload now rewrites the map). Mixed old/new cluster behaviour measured with `main` binaries and written up as design §4.4.1 for the PR note; §9 and the open-questions row updated. Full UDS/config/reload slice green under `-race`; CI `srv_pkg_non_js_tests` shard attempted four times (three on the branch, one on clean `main`): every attempt aborts on a pre-existing fixed-port bind panic or `TestPSEmulationCPU`, identically on `main`, so a clean full run must come from GitHub CI. | Dave: GitHub issue, real-proxy evidence, PR description (mixed-version paragraph from §4.4.1), decide squash vs. per-phase. |
