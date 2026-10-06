// Copyright 2026 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package server

// Design 58 P2e-2 flag-on proof: a two-node cluster whose route transport is
// tcpzc (io_uring SEND_ZC/recv over an ordinary loopback TCP route) forms end to
// end and carries real NATS pub/sub across the route. It exercises the whole
// wiring stack added in P2e-2 — Cluster.TCPZC parse, the accept/dial fd-wrap
// (openTCPZCConn), the tcpzc backend's ring loop, and the reused
// readLoopFast/flushOutboundFast route handshake. It needs io_uring at runtime,
// so it is linux-only and skips when a ring cannot be created (older kernel /
// restricted sandbox — e.g. it is NOT run by .#ci-local, which has no io_uring).

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/randomizedcoder/uds-rdma-proxy/tools/urp-fast-go/tcpzc"
)

// tcpzcAvailable reports whether io_uring can back a tcpzc.Conn on this host. It
// opens a throwaway ring over one end of a socketpair; ErrNoIoUring (or any open
// failure) means the environment cannot run the tcpzc route and the caller
// should skip. The probe closes whatever it successfully opened.
func tcpzcAvailable(t *testing.T) bool {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	// tcpzc.Open takes ownership of fds[0] and closes it on any error path.
	conn, err := tcpzc.Open(fds[0], tcpzc.Config{BufSize: 4096, Count: 8})
	if err != nil {
		_ = unix.Close(fds[1])
		return false
	}
	_ = conn.Close()
	_ = unix.Close(fds[1])
	return true
}

func TestTCPZCRouteClusterFormsAndCarriesTraffic(t *testing.T) {
	if !tcpzcAvailable(t) {
		t.Skip("io_uring not available; skipping tcpzc route integration test")
	}

	// Disable route pooling so the cluster is a single point-to-point route
	// each way (PoolSize < 0). This matches the fast path's one-endpoint model
	// and makes the formed-route count deterministic. Pooled tcpzc (multiple
	// parallel route connections) is a separate follow-up.
	optsA := DefaultOptions()
	optsA.ServerName = "A"
	optsA.Cluster.Name = "tcpzc-test"
	optsA.Cluster.Host = "127.0.0.1"
	optsA.Cluster.PoolSize = -1
	optsA.Cluster.TCPZC = true
	srvA := RunServer(optsA)
	defer srvA.Shutdown()

	routeA := fmt.Sprintf("nats://127.0.0.1:%d", srvA.ClusterAddr().Port)

	optsB := DefaultOptions()
	optsB.ServerName = "B"
	optsB.Cluster.Name = "tcpzc-test"
	optsB.Cluster.Host = "127.0.0.1"
	optsB.Cluster.PoolSize = -1
	optsB.Cluster.TCPZC = true
	optsB.Routes = RoutesFromStr(routeA)
	srvB := RunServer(optsB)
	defer srvB.Shutdown()

	// The route must form over the tcpzc transport (dial side on B wraps its
	// dialed fd, accept side on A wraps the accepted fd).
	checkClusterFormed(t, srvA, srvB)

	// Both route ends must report the tcpzc transport, not "fast" or "tcp".
	checkRouteTransport(t, srvA, routeTransportTCPZC)
	checkRouteTransport(t, srvB, routeTransportTCPZC)

	// A subscriber on B must receive a message published on A: this only works
	// if the route actually carries the SUB interest (A->... no, B->A) and the
	// MSG (A->B) across the tcpzc endpoints.
	ncB := natsConnect(t, srvB.ClientURL())
	defer ncB.Close()
	sub, err := ncB.SubscribeSync("tcpzc.subject")
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	if err := ncB.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Wait for the SUB interest to propagate A<-B over the route.
	checkSubInterest(t, srvA, globalAccountName, "tcpzc.subject", 2*time.Second)

	ncA := natsConnect(t, srvA.ClientURL())
	defer ncA.Close()
	payload := []byte("hello over tcpzc")
	natsPub(t, ncA, "tcpzc.subject", payload)
	natsFlush(t, ncA)

	msg := natsNexMsg(t, sub, 2*time.Second)
	if string(msg.Data) != string(payload) {
		t.Fatalf("payload = %q, expected %q", msg.Data, payload)
	}
}

// TestTCPZCRoutePoolEveryMemberCompletes is the design 58 P2e-5f reproduction.
//
// The integration test above sets Cluster.PoolSize = -1, so until now NO test had
// ever run tcpzc with route POOLING — which is the default (DEFAULT_ROUTE_POOL_SIZE
// = 3) and therefore exactly what the NixOS module and the HW A/B actually ran. On
// hp1/hp2/hp3 that configuration left one route per remote that never answered a
// PING (rtt=0s, subs=0), so NATS closed it as a Stale Connection every ~31s and
// re-dialled forever — 466 stale route connections in 2h7m — and the JetStream
// drive hung for over two hours.
//
// Two assertions matter, and neither exists above. checkClusterFormed counts the
// routes a pooled cluster is SUPPOSED to have (pool members plus any dedicated
// account routes), so a member that never completes makes it time out. And a route
// that completes but then dies has to be caught by watching for longer than the
// stale deadline, which is why routeMaxPingInterval is shortened here: it is what
// turns the HW's ~31s churn period into a sub-second one.
func TestTCPZCRoutePoolEveryMemberCompletes(t *testing.T) {
	if !tcpzcAvailable(t) {
		t.Skip("io_uring not available; skipping tcpzc route pool test")
	}

	// Shorten the route stale deadline (pingInterval * (MaxPingsOut+1)) from ~90s
	// to ~750ms so the churn is observable inside a test. Restored for the rest of
	// the package.
	orgMaxPing := routeMaxPingInterval
	routeMaxPingInterval = 250 * time.Millisecond
	defer func() { routeMaxPingInterval = orgMaxPing }()

	for _, tc := range []struct {
		description     string
		poolSize        int
		noSystemAccount bool
		expected        string
	}{
		{
			description: "POS pooling disabled: the only shape P2e-2 ever tested",
			poolSize:    -1,
			expected:    "one route each way, and none of them goes stale",
		},
		{
			description: "NEG default pooling: what the NixOS module and the HW A/B run",
			poolSize:    0, // -> DEFAULT_ROUTE_POOL_SIZE
			expected:    "every pool member plus the dedicated $SYS route completes, and none goes stale",
		},
		{
			description: "BND a pool of exactly one",
			poolSize:    1,
			expected:    "the single pool member plus the dedicated $SYS route completes, and neither goes stale",
		},
		{
			description:     "COR pooling with no dedicated $SYS route",
			poolSize:        3,
			noSystemAccount: true,
			expected:        "all three pool members complete with no account route, and none goes stale",
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			mk := func(name string) *Options {
				o := DefaultOptions()
				o.ServerName = name
				o.Cluster.Name = "tcpzc-pool-test"
				o.Cluster.Host = "127.0.0.1"
				o.Cluster.PoolSize = tc.poolSize
				o.Cluster.TCPZC = true
				o.NoSystemAccount = tc.noSystemAccount
				return o
			}

			optsA := mk("A")
			srvA := RunServer(optsA)
			defer srvA.Shutdown()

			optsB := mk("B")
			optsB.Routes = RoutesFromStr(fmt.Sprintf("nats://127.0.0.1:%d", srvA.ClusterAddr().Port))
			srvB := RunServer(optsB)
			defer srvB.Shutdown()

			// Every route the pooled cluster is supposed to have must complete. A
			// member whose handshake never round-trips never gets counted here.
			checkClusterFormed(t, srvA, srvB)

			if err := checkRoutesStayUp(srvA, srvB, 3*time.Second); err != nil {
				t.Fatalf("%s: %v (expected: %s)", tc.description, err, tc.expected)
			}
		})
	}
}

// TestTCPZCPollTimeoutFromEnv covers the knob added so the io_uring wait window
// can be swept on hardware. It matters more than it looks: NATS runs one ring, and
// so one ioLoop, per ROUTE -- 8 per node on a 3-node cluster with the default route
// pool -- so the idle wakeup rate is multiplied by the cluster and lands directly
// in the syscalls/msg metric. The one value that must never be accepted is 0,
// because PollOnce reads timeout <= 0 as "non-blocking" and the loop becomes a
// userspace spin.
func TestTCPZCPollTimeoutFromEnv(t *testing.T) {
	for _, tc := range []struct {
		description string
		env         string
		set         bool
		expected    time.Duration
	}{
		{
			description: "POS unset falls back to the default",
			set:         false,
			expected:    time.Duration(tcpzcDefaultPollUS) * time.Microsecond,
		},
		{
			description: "POS a plain microsecond count is honoured",
			env:         "5000",
			set:         true,
			expected:    5 * time.Millisecond,
		},
		{
			description: "NEG a non-numeric value falls back rather than disabling the wait",
			env:         "fast-please",
			set:         true,
			expected:    time.Duration(tcpzcDefaultPollUS) * time.Microsecond,
		},
		{
			description: "BND zero is refused: it would turn the loop into a userspace spin",
			env:         "0",
			set:         true,
			expected:    time.Duration(tcpzcDefaultPollUS) * time.Microsecond,
		},
		{
			description: "BND one microsecond is a legal (if aggressive) window",
			env:         "1",
			set:         true,
			expected:    time.Microsecond,
		},
		{
			description: "COR an empty string is treated as unset",
			env:         _EMPTY_,
			set:         true,
			expected:    time.Duration(tcpzcDefaultPollUS) * time.Microsecond,
		},
		{
			description: "COR a negative value falls back (ParseUint rejects the sign)",
			env:         "-200",
			set:         true,
			expected:    time.Duration(tcpzcDefaultPollUS) * time.Microsecond,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if tc.set {
				t.Setenv("NATS_TCPZC_POLL_US", tc.env)
			} else {
				os.Unsetenv("NATS_TCPZC_POLL_US")
			}
			if got := tcpzcPollTimeoutFromEnv(); got != tc.expected {
				t.Fatalf("%s: poll timeout = %v, expected %v", tc.description, got, tc.expected)
			}
		})
	}
}

// TestTCPZCWaitNrFromEnv covers the completion-batch knob. It is the knob that
// decides whether io_uring is batching at all: at 1 the loop asks the kernel to
// return on the FIRST completion, which measured 0.19-0.27 messages per enter on a
// three-node cluster -- about five enters per message, strictly worse than a plain
// read(). The default must stay 1 so the hardware A/B compares against the numbers
// already recorded, and the library clamps anything too large, so the only thing
// this parse has to get right is never silently producing 0 (which would mean
// "non-blocking" and is a different code path entirely). Design 58 P2e-5m.
func TestTCPZCWaitNrFromEnv(t *testing.T) {
	for _, tc := range []struct {
		description string
		env         string
		set         bool
		expected    uint32
	}{
		{
			description: "POS unset keeps the pre-P2e-5m behaviour",
			set:         false,
			expected:    tcpzc.DefaultWaitNr,
		},
		{
			description: "POS the batch size the user's thesis names is accepted verbatim",
			env:         "64",
			set:         true,
			expected:    64,
		},
		{
			description: "POS the other named batch size is accepted verbatim",
			env:         "128",
			set:         true,
			expected:    128,
		},
		{
			description: "NEG a non-numeric value falls back rather than disabling the wait",
			env:         "lots",
			set:         true,
			expected:    tcpzc.DefaultWaitNr,
		},
		{
			description: "BND zero is refused: it must not become the non-blocking path",
			env:         "0",
			set:         true,
			expected:    tcpzc.DefaultWaitNr,
		},
		{
			description: "BND one is legal and is the default",
			env:         "1",
			set:         true,
			expected:    1,
		},
		{
			description: "BND a value past any ring depth parses; the library clamps it, not this",
			env:         "100000",
			set:         true,
			expected:    100000,
		},
		{
			description: "COR an empty string is treated as unset",
			env:         _EMPTY_,
			set:         true,
			expected:    tcpzc.DefaultWaitNr,
		},
		{
			description: "COR a negative value falls back (ParseUint rejects the sign)",
			env:         "-8",
			set:         true,
			expected:    tcpzc.DefaultWaitNr,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if tc.set {
				t.Setenv("NATS_TCPZC_WAIT_NR", tc.env)
			} else {
				os.Unsetenv("NATS_TCPZC_WAIT_NR")
			}
			if got := tcpzcWaitNrFromEnv(); got != tc.expected {
				t.Fatalf("%s: waitNr = %d, expected %d", tc.description, got, tc.expected)
			}
		})
	}
}

// checkRoutesStayUp samples the servers for d and fails if any route connection
// goes stale or the route count moves. A tcpzc route that completes its handshake
// and then stops answering PINGs looks perfectly healthy the instant after the
// cluster forms, so only a window longer than the stale deadline can see it.
func checkRoutesStayUp(srvA, srvB *Server, d time.Duration) error {
	startA, startB := srvA.NumRoutes(), srvB.NumRoutes()
	staleA, staleB := srvA.NumStaleConnectionsRoutes(), srvB.NumStaleConnectionsRoutes()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		for _, s := range []struct {
			srv   *Server
			route int
			stale uint64
		}{{srvA, startA, staleA}, {srvB, startB, staleB}} {
			if got := s.srv.NumStaleConnectionsRoutes(); got != s.stale {
				return fmt.Errorf("%s: %d route connections went stale (was %d, now %d) — the route is churning",
					s.srv.Name(), got-s.stale, s.stale, got)
			}
			if got := s.srv.NumRoutes(); got != s.route {
				return fmt.Errorf("%s: route count moved %d -> %d — a route was torn down or re-dialled",
					s.srv.Name(), s.route, got)
			}
		}
	}
	return nil
}

// checkRouteTransport asserts the server has at least one route and that every
// route reports the given transport name.
func checkRouteTransport(t *testing.T, s *Server, want string) {
	t.Helper()
	checkFor(t, 2*time.Second, 15*time.Millisecond, func() error {
		var seen int
		var bad string
		s.mu.RLock()
		s.forEachRoute(func(r *client) {
			seen++
			r.mu.Lock()
			if r.route != nil && r.route.transport != want {
				bad = r.route.transport
			}
			r.mu.Unlock()
		})
		s.mu.RUnlock()
		if seen == 0 {
			return fmt.Errorf("%s: no routes yet", s.Name())
		}
		if bad != _EMPTY_ {
			return fmt.Errorf("%s: route transport=%q, want %q", s.Name(), bad, want)
		}
		return nil
	})
}

// TestTCPZCDebugLine covers the dump's pure formatter and, crucially, the verdict
// it selects. The verdict is the entire value of the instrument: a wall of counters
// still has to be interpreted, and interpreting them by hand is what produced
// P2e-5k. Design 58 P2e-5l.
func TestTCPZCDebugLine(t *testing.T) {
	const sec = time.Second

	for _, tc := range []struct {
		description string
		name        string
		delta       tcpzc.Stats
		elapsed     time.Duration
		expected    string // substring that must appear
	}{
		{
			description: "POS a quiet route reports no spin",
			name:        "route-hp2",
			delta:       tcpzc.Stats{Syscalls: 5000, RecvDelivered: 1200, BufsReturned: 1200, RingAvail: 128, RecvArmed: true},
			elapsed:     sec,
			expected:    "tcpzc-dbg name=route-hp2 spin=none ms=1000 enters=5000 msgs=1200",
		},
		{
			description: "POS a hot loop with nothing to show for it is named unattributed, the verdict no profile could give",
			name:        "route-hp3",
			delta:       tcpzc.Stats{Syscalls: 1070000, RecvDelivered: 40},
			elapsed:     sec,
			expected:    "spin=unattributed",
		},
		{
			description: "POS arms coming straight back as -ENOBUFS with nothing delivered is named",
			name:        "r",
			delta:       tcpzc.Stats{Syscalls: 541000, ArmSubmitted: 540000, RecvENOBUFS: 540000, RecvDelivered: 10},
			elapsed:     sec,
			expected:    "spin=enobufs-rearm",
		},
		{
			description: "NEG the measured healthy shape -- drowning in -ENOBUFS but carrying >=1 msg per enter -- is NOT a spin",
			name:        "r",
			delta:       tcpzc.Stats{Syscalls: 166200, RecvDelivered: 1329133, RecvENOBUFS: 7002800, ArmRefused: 124100},
			elapsed:     sec,
			expected:    "spin=none",
		},
		{
			description: "BND an interval just under the attribution floor is not attributed however it is shaped",
			name:        "r",
			delta:       tcpzc.Stats{Syscalls: tcpzcDebugSpinFloorPerSec - 1, RecvENOBUFS: tcpzcDebugSpinFloorPerSec - 1},
			elapsed:     sec,
			expected:    "spin=none",
		},
		{
			description: "BND the floor scales with elapsed time, so a half-second interval halves it",
			name:        "r",
			delta:       tcpzc.Stats{Syscalls: tcpzcDebugSpinFloorPerSec / 2, RecvENOBUFS: tcpzcDebugSpinFloorPerSec / 2},
			elapsed:     sec / 2,
			expected:    "spin=enobufs-rearm ms=500",
		},
		{
			description: "COR a starved ring reports its live state so a stall is distinguishable from a spin",
			name:        "r",
			delta:       tcpzc.Stats{Syscalls: 5000, RingAvail: 0, RecvArmed: false, ArmRefused: 3},
			elapsed:     sec,
			expected:    "ringAvail=0 isArmed=false",
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			got := tcpzcDebugLine(tc.name, tc.delta, tc.elapsed)
			if !strings.Contains(got, tc.expected) {
				t.Fatalf("%s:\n  got      %q\n  expected to contain %q", tc.description, got, tc.expected)
			}
			if !strings.HasSuffix(got, "\n") {
				t.Fatalf("%s: line is not newline-terminated, so journal lines would run together: %q",
					tc.description, got)
			}
		})
	}
}

// TestTCPZCDebugDumpReachesTheSink drives a REAL ring and requires a line to
// actually land. The formatter being correct is not the risk; the risk is that the
// gate never fires and the dump stays silent, which is exactly how the first
// hardware attempt was wasted (the scrape also died of SIGPIPE, and --collect took
// the journals with it, so nothing could be checked after the fact). Design 58
// P2e-5l.
func TestTCPZCDebugDumpReachesTheSink(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type acc struct {
		c   net.Conn
		err error
	}
	accCh := make(chan acc, 1)
	go func() {
		c, aerr := ln.Accept()
		accCh <- acc{c, aerr}
	}()
	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	a := <-accCh
	if a.err != nil {
		t.Fatalf("accept: %v", a.err)
	}

	dfd, err := ownFdFromConn(dialed)
	if err != nil {
		t.Fatalf("own dialed fd: %v", err)
	}
	afd, err := ownFdFromConn(a.c)
	if err != nil {
		t.Fatalf("own accepted fd: %v", err)
	}

	// Report every iteration into a buffer, on a short interval, so the test does
	// not need a second of traffic. Restored before returning.
	var mu sync.Mutex
	buf := &lockedBuffer{mu: &mu}
	origDebug, origIval, origMask, origOut := tcpzcDebug, tcpzcDebugInterval, tcpzcDebugIterMask, tcpzcDebugOut
	tcpzcDebug, tcpzcDebugInterval, tcpzcDebugIterMask, tcpzcDebugOut = true, 5*time.Millisecond, 0, buf
	defer func() {
		tcpzcDebug, tcpzcDebugInterval, tcpzcDebugIterMask, tcpzcDebugOut = origDebug, origIval, origMask, origOut
	}()

	ep1, err := openTCPZCEndpointFd(dfd, "dbg-dialed")
	if err != nil {
		t.Skipf("tcpzc endpoint unavailable here (no io_uring?): %v", err)
	}
	defer ep1.Close()
	ep2, err := openTCPZCEndpointFd(afd, "dbg-accepted")
	if err != nil {
		t.Skipf("tcpzc endpoint unavailable here (no io_uring?): %v", err)
	}
	defer ep2.Close()

	// A little real traffic, so the counters are not all zero.
	idx, b, err := ep1.SendBuf()
	if err != nil {
		t.Fatalf("SendBuf: %v", err)
	}
	copy(b, []byte("PING\r\n"))
	if err := ep1.Send(idx, 6); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if s := buf.String(); strings.Contains(s, "tcpzc-dbg name=dbg-dialed ") {
			got = s
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == "" {
		t.Fatalf("no tcpzc-dbg line reached the sink in 5s with the dump enabled — the diagnostic is silent, which is worse than absent (buffer: %q)", buf.String())
	}
	// It must carry a verdict, or the scrape has nothing to tally.
	if !strings.Contains(got, "spin=") {
		t.Fatalf("dump line carries no spin verdict: %q", got)
	}
	t.Logf("dump line landed: %s", strings.SplitN(got, "\n", 2)[0])
}

// lockedBuffer is a bytes.Buffer safe for the ioLoop goroutine to write while the
// test reads it.
type lockedBuffer struct {
	mu *sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
