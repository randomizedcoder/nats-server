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
	"fmt"
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
