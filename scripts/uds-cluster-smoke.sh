#!/usr/bin/env bash
#
# Three-process smoke test for NATS cluster routes over Unix domain sockets.
#
# Builds nats-server, starts servers A, B and C from test/configs/uds/, waits
# for the full mesh to form over unix sockets, captures `ss` evidence, pushes
# traffic across the mesh, crashes and restarts B to exercise stale-socket
# removal, then shuts everything down and checks that no socket files remain.
#
# Usage:
#   scripts/uds-cluster-smoke.sh
#   UDS_PAIRS=1 scripts/uds-cluster-smoke.sh   # ring through socat proxy pairs
#
# Environment:
#   UDS_PAIRS   0 (default) full mesh, every server dials the other two.
#               1 ring: each server dials one proxy socket; socat forwards it
#               to the next server, standing in for uds-over-rdma-proxy.
#   UDS_PROXY   1 with UDS_PAIRS=1 to skip starting socat because an external
#               proxy (uds-over-rdma-proxy) already serves ab/bc/ca.sock.
#   UDS_DIR     socket directory (default /tmp/nats-uds; keep it short).
#   OUT         output directory for binary, logs and evidence (default mktemp).
#   NATS_CLI    nats CLI command (default: nats from PATH, else nix shell).
#   BENCH_MSGS  messages for nats bench (default 100000).
#   TIMEOUT     seconds to wait for the mesh to form (default 10).
#
# Requirements: go, curl, ss (iproute2), the nats CLI, and socat for UDS_PAIRS=1.

set -euo pipefail

UDS_PAIRS=${UDS_PAIRS:-0}
UDS_PROXY=${UDS_PROXY:-0}
UDS_DIR=${UDS_DIR:-/tmp/nats-uds}
OUT=${OUT:-$(mktemp -d /tmp/nats-uds-out.XXXXXX)}
BENCH_MSGS=${BENCH_MSGS:-100000}
TIMEOUT=${TIMEOUT:-10}

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CONF_DIR=$REPO/test/configs/uds
SERVERS=(a b c)
declare -A CLIENT_PORT=([a]=14222 [b]=14223 [c]=14224)
declare -A HTTP_PORT=([a]=18222 [b]=18223 [c]=18224)
declare -A PID=()
SOCAT_PIDS=()
PASS=0
FAIL=0
FAILED_STEPS=()

log()  { printf '%s %s\n' "$(date +%H:%M:%S)" "$*"; }
pass() { PASS=$((PASS + 1)); log "PASS: $*"; }
fail() { FAIL=$((FAIL + 1)); FAILED_STEPS+=("$*"); log "FAIL: $*"; }
die()  { log "FATAL: $*"; exit 1; }

# evidence NAME CMD...: run CMD, save its output to $OUT/NAME.txt and echo it
# so it can be pasted into a PR.
evidence() {
	local name=$1
	shift
	log "--- $name ($*)"
	"$@" 2>&1 | tee "$OUT/$name.txt" || true
	log "--- end $name"
}

# show FILE: echo an evidence file that was already saved under $OUT.
show() {
	log "--- $(basename "$1")"
	cat "$1"
	log "--- end $(basename "$1")"
}

cleanup() {
	local p
	for p in "${PID[@]:-}" "${SOCAT_PIDS[@]:-}"; do
		[ -n "$p" ] && kill "$p" 2>/dev/null || true
	done
}
trap cleanup EXIT

resolve_nats_cli() {
	if [ -n "${NATS_CLI:-}" ]; then
		return
	fi
	if command -v nats >/dev/null 2>&1; then
		NATS_CLI=nats
	elif command -v nix >/dev/null 2>&1; then
		NATS_CLI="nix shell nixpkgs#natscli -c nats"
	else
		die "nats CLI not found; set NATS_CLI"
	fi
}

conf_for() {
	local n=$1
	if [ "$UDS_PAIRS" = 1 ]; then
		echo "$CONF_DIR/srv_${n}_pairs.conf"
	else
		echo "$CONF_DIR/srv_${n}.conf"
	fi
}

# The checked-in configs hardcode /tmp/nats-uds. Rewrite copies when UDS_DIR
# points somewhere else.
prepare_conf() {
	local n=$1 src dst
	src=$(conf_for "$n")
	if [ "$UDS_DIR" = /tmp/nats-uds ]; then
		echo "$src"
		return
	fi
	dst=$OUT/$(basename "$src")
	sed "s#/tmp/nats-uds#$UDS_DIR#g" "$src" >"$dst"
	echo "$dst"
}

start_server() {
	local n=$1 conf
	conf=$(prepare_conf "$n")
	"$OUT/nats-server" -c "$conf" -l "$OUT/$n.log" &
	PID[$n]=$!
	log "started $n pid ${PID[$n]} ($conf)"
}

# monitor NAME PATH: fetch a monitoring endpoint, logging failures.
monitor() {
	local rc=0
	curl -sf "http://127.0.0.1:${HTTP_PORT[$1]}$2" || rc=$?
	[ $rc = 0 ] || log "curl $1 $2 failed rc=$rc"
	return $rc
}
routez() { monitor "$1" /routez; }
varz()   { monitor "$1" /varz; }

# mesh_summary NAME: prints "num_routes peers unix_routes" for NAME. With the
# default route pool each peer pair carries several connections, so the mesh
# is judged by distinct peers rather than by num_routes.
mesh_summary() {
	local body routes peers unix
	body=$(routez "$1" 2>/dev/null) || return 1
	routes=$(printf '%s' "$body" | grep -o '"num_routes": *[0-9]*' | grep -o '[0-9]*$' || echo 0)
	peers=$(printf '%s' "$body" | grep -o '"remote_name": *"[^"]*"' | sort -u | wc -l)
	unix=$(printf '%s' "$body" | grep -o '"transport": *"unix"' | wc -l)
	echo "$routes $peers $unix"
}

# mesh_ready NAME: true when NAME sees both other servers and every route is unix.
mesh_ready() {
	local routes peers unix
	read -r routes peers unix < <(mesh_summary "$1") || return 1
	[ "$peers" = 2 ] && [ "$routes" -gt 0 ] && [ "$unix" = "$routes" ]
}

# Pretty-printed /varz uses two-space indentation; cut the top-level
# "cluster" object out by its indentation.
varz_cluster() {
	varz "$1" >"$OUT/varz-$1-$2.json" || return 1
	sed -n '/^  "cluster": {/,/^  },\{0,1\}$/p' "$OUT/varz-$1-$2.json"
}

wait_for_mesh() {
	local deadline=$((SECONDS + TIMEOUT)) n ok
	while [ $SECONDS -lt $deadline ]; do
		ok=1
		for n in "${SERVERS[@]}"; do
			mesh_ready "$n" || ok=0
		done
		[ $ok = 1 ] && return 0
		sleep 0.25
	done
	for n in "${SERVERS[@]}"; do
		log "routez $n (num_routes peers unix): $(mesh_summary "$n" 2>&1 || echo unavailable)"
	done
	return 1
}

start_proxy_pairs() {
	# ab.sock -> b.sock, bc.sock -> c.sock, ca.sock -> a.sock
	local pair
	for pair in ab:b bc:c ca:a; do
		local near=${pair%%:*} far=${pair##*:}
		socat "UNIX-LISTEN:$UDS_DIR/$near.sock,fork,unlink-early" \
			"UNIX-CONNECT:$UDS_DIR/$far.sock" >>"$OUT/socat.log" 2>&1 &
		SOCAT_PIDS+=($!)
		log "socat pair $near.sock -> $far.sock pid $!"
	done
	local deadline=$((SECONDS + TIMEOUT))
	while [ $SECONDS -lt $deadline ]; do
		[ -S "$UDS_DIR/ab.sock" ] && [ -S "$UDS_DIR/bc.sock" ] && [ -S "$UDS_DIR/ca.sock" ] && return 0
		sleep 0.1
	done
	die "socat proxy sockets did not appear in $UDS_DIR"
}

# ---------------------------------------------------------------------------

log "output directory: $OUT"
log "socket directory: $UDS_DIR (UDS_PAIRS=$UDS_PAIRS)"
mkdir -p "$UDS_DIR"
if ls "$UDS_DIR"/*.sock >/dev/null 2>&1 && [ "$UDS_PROXY" != 1 ]; then
	die "$UDS_DIR already contains socket files; remove them or set UDS_DIR"
fi
command -v ss >/dev/null || die "ss (iproute2) not found"
command -v curl >/dev/null || die "curl not found"
if [ "$UDS_PAIRS" = 1 ] && [ "$UDS_PROXY" != 1 ]; then
	command -v socat >/dev/null || die "socat not found (needed for UDS_PAIRS=1)"
fi
resolve_nats_cli
log "nats CLI: $NATS_CLI"

# 1. Build.
log "building nats-server"
(cd "$REPO" && go build -o "$OUT/nats-server" .)
"$OUT/nats-server" --version | tee "$OUT/version.txt"

# 2. Config check.
for n in "${SERVERS[@]}"; do
	conf=$(prepare_conf "$n")
	if "$OUT/nats-server" -t -c "$conf" >>"$OUT/config-check.txt" 2>&1; then
		pass "config check $(basename "$conf")"
	else
		fail "config check $(basename "$conf")"
		cat "$OUT/config-check.txt"
		exit 1
	fi
done

# 3. Start.
if [ "$UDS_PAIRS" = 1 ] && [ "$UDS_PROXY" != 1 ]; then
	# Start servers first so the proxies have listeners to connect to, but
	# the servers' own dials will fail until the proxy sockets exist.
	for n in "${SERVERS[@]}"; do start_server "$n"; done
	start_proxy_pairs
else
	for n in "${SERVERS[@]}"; do start_server "$n"; done
fi

# 4. Wait for the mesh.
if wait_for_mesh; then
	pass "mesh formed: every server sees 2 peers, all routes transport unix"
else
	fail "mesh did not form within ${TIMEOUT}s"
fi

# 5. Evidence.
evidence ss-listen bash -c "ss -xlp | grep -F '$UDS_DIR'"
evidence ss-established bash -c "ss -xp | grep -F '$UDS_DIR'"
listeners=$(ss -xlp | grep -F "$UDS_DIR" | grep -c nats-server || true)
if [ "$listeners" -ge 3 ]; then
	pass "ss -xlp shows $listeners nats-server unix listeners"
else
	fail "ss -xlp shows $listeners nats-server unix listeners, expected 3"
fi
for n in "${SERVERS[@]}"; do
	routez "$n" >"$OUT/routez-$n.json" || true
	log "routez $n (num_routes peers unix): $(mesh_summary "$n")"
	varz_cluster "$n" mesh >"$OUT/varz-cluster-$n.txt" || true
	show "$OUT/varz-cluster-$n.txt"
	if grep -q '"unix_socket_stats"' "$OUT/varz-cluster-$n.txt"; then
		pass "varz $n reports unix_socket_stats"
	else
		fail "varz $n missing unix_socket_stats"
	fi
done
if [ "$UDS_PAIRS" = 1 ]; then
	if ss -xp | grep -F "$UDS_DIR" | grep -q socat; then
		pass "ss -xp shows the proxy (socat) on the far end of the route sockets"
	else
		fail "ss -xp does not show socat on any route socket"
	fi
fi

# 6. Traffic: subscribe on A, publish on C.
subout=$OUT/sub-a.txt
$NATS_CLI -s "nats://127.0.0.1:${CLIENT_PORT[a]}" sub uds.smoke --count=1 >"$subout" 2>&1 &
subpid=$!
received=0
for _ in $(seq 1 40); do
	$NATS_CLI -s "nats://127.0.0.1:${CLIENT_PORT[c]}" pub uds.smoke "hello over unix routes" >/dev/null 2>&1 || true
	sleep 0.25
	if ! kill -0 "$subpid" 2>/dev/null; then
		received=1
		break
	fi
done
if [ $received = 1 ] && grep -q "hello over unix routes" "$subout"; then
	pass "message published on C was delivered to the subscriber on A"
else
	kill "$subpid" 2>/dev/null || true
	fail "cross-server pub/sub (see $subout)"
fi

log "nats bench: $BENCH_MSGS messages, subscriber on B, publisher on A"
$NATS_CLI -s "nats://127.0.0.1:${CLIENT_PORT[b]}" bench sub uds.bench --msgs "$BENCH_MSGS" --no-progress \
	>"$OUT/bench-sub-b.txt" 2>&1 &
benchpid=$!
sleep 1
$NATS_CLI -s "nats://127.0.0.1:${CLIENT_PORT[a]}" bench pub uds.bench --msgs "$BENCH_MSGS" --size 128B --no-progress \
	>"$OUT/bench-pub-a.txt" 2>&1 || true
if wait "$benchpid"; then
	pass "nats bench completed"
	show "$OUT/bench-sub-b.txt"
else
	fail "nats bench subscriber did not complete (see $OUT/bench-sub-b.txt)"
fi

# 7. SIGKILL B, leaving its socket file behind, and restart it.
log "SIGKILL server b (pid ${PID[b]})"
kill -9 "${PID[b]}"
wait "${PID[b]}" 2>/dev/null || true
if [ -S "$UDS_DIR/b.sock" ]; then
	pass "b.sock left behind after SIGKILL"
else
	fail "b.sock was not left behind after SIGKILL"
fi
mv "$OUT/b.log" "$OUT/b-before-kill.log"
start_server b
if wait_for_mesh; then
	pass "mesh re-formed after restarting b"
else
	fail "mesh did not re-form after restarting b"
fi
if grep -q "Removed stale unix socket" "$OUT/b.log"; then
	pass "restarted b logged stale socket removal: $(grep -m1 'Removed stale unix socket' "$OUT/b.log" | sed 's/.*\[WRN\] //')"
else
	fail "restarted b did not log 'Removed stale unix socket'"
fi
varz_cluster b restart >"$OUT/varz-cluster-b-restart.txt" || true
show "$OUT/varz-cluster-b-restart.txt"
if grep -q '"stale_removed": *1' "$OUT/varz-cluster-b-restart.txt"; then
	pass "varz b reports stale_removed 1"
else
	fail "varz b does not report stale_removed 1"
fi
evidence ss-established-after-restart bash -c "ss -xp | grep -F '$UDS_DIR'"

# 8. SIGTERM all and check no socket files remain.
for n in "${SERVERS[@]}"; do
	kill -TERM "${PID[$n]}"
done
for n in "${SERVERS[@]}"; do
	wait "${PID[$n]}" 2>/dev/null || true
	PID[$n]=""
done
for p in "${SOCAT_PIDS[@]:-}"; do
	[ -n "$p" ] && kill "$p" 2>/dev/null || true
done
SOCAT_PIDS=()
leftover=""
for n in "${SERVERS[@]}"; do
	[ -e "$UDS_DIR/$n.sock" ] && leftover="$leftover $n.sock"
done
if [ -z "$leftover" ]; then
	pass "no server socket files remain in $UDS_DIR after SIGTERM"
else
	fail "socket files remain after SIGTERM:$leftover"
fi

# 9. Summary.
log "passed: $PASS failed: $FAIL (logs and evidence in $OUT)"
if [ $FAIL -ne 0 ]; then
	printf '  failed: %s\n' "${FAILED_STEPS[@]}"
	exit 1
fi
