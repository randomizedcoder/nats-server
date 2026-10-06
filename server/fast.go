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

// Fast endpoint (design 58 P2): a first-class zero-copy route transport that
// sends and receives whole messages by pool-buffer index, with no payload copy
// at the transport seam. Unlike TCP and the unix-socket route transport, a fast
// endpoint is NOT a net.Conn: a net.Conn shim would Read/Write-copy the pool
// buffer to and from the caller and reintroduce the very copy this design
// removes (build spec §8.4-B, rejected). Instead the client gains a fast branch
// (client.fast) alongside nc net.Conn, driven by the readLoop/writeLoop seams
// (§4/§5, wired in P2b/P2c). In P2a the loops still run on TCP; this file lands
// the transport-generic seam, the fastConn bridge, and createFastClient so the
// fork builds flag-off byte-identical and the seam is proven by loopback.

package server

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// routeTransportFast is the transport name a fast-endpoint route reports to
// /varz, /routez and STATSZ, alongside routeTransportTCP / routeTransportUnix.
const routeTransportFast = "fast"

// routeTransportTCPZC is the transport name a tcpzc route reports. A tcpzc route
// is wire-identical to a plain TCP route (io_uring SEND_ZC/recv is a purely local
// drive choice); the distinct name lets /varz and the A/B harness tell a tcpzc
// cluster from a plain-TCP one (design 58 §9a).
const routeTransportTCPZC = "tcpzc"

// Fast-endpoint route addressing (design 58 P2, route model §3).
//
// A solicited fast route is written as a URL "urp://<endpoint-name>", the fast
// analog of the unix-socket "unix://<path>" form (uds.go). Unlike a unix path,
// a urp endpoint name is a bare token (no slash), so it is carried in the URL
// Host rather than Path; url.Parse("urp://name") yields Host="name" and
// String() round-trips to "urp://name". The name obeys the same 15-byte wire
// cap as cluster.fast_endpoint (validateFastEndpointName), because both name
// the same urp REGISTER endpoint. There is no host/port and no gossip: a fast
// route is pre-provisioned point-to-point (route model §4), so a fast server
// never advertises a urp:// URL for peers to auto-discover.

// fastSchemePrefix is the fast route scheme prefix, compared case-insensitively.
const fastSchemePrefix = "urp://"

// hasFastScheme reports whether s starts with "urp://", ignoring case.
func hasFastScheme(s string) bool {
	return len(s) >= len(fastSchemePrefix) &&
		strings.EqualFold(s[:len(fastSchemePrefix)], fastSchemePrefix)
}

// parseFastAddr validates a "urp://" route address from configuration or the
// command line and returns the endpoint name after the scheme. The name is
// held to the urp wire cap (validateFastEndpointName) so a route URL and a
// cluster.fast_endpoint that name the same endpoint validate identically.
func parseFastAddr(raw string) (string, error) {
	if strings.TrimSpace(raw) != raw {
		return _EMPTY_, fmt.Errorf("fast endpoint address %q must not have leading or trailing spaces", raw)
	}
	if !hasFastScheme(raw) {
		return _EMPTY_, fmt.Errorf("fast endpoint address %q must start with %q", raw, fastSchemePrefix)
	}
	name := raw[len(fastSchemePrefix):]
	if err := validateFastEndpointName(name); err != nil {
		return _EMPTY_, fmt.Errorf("fast endpoint address %q: %w", raw, err)
	}
	// A slash, query or fragment would not survive as a bare urp endpoint name.
	if strings.ContainsAny(name, "/?#") {
		return _EMPTY_, fmt.Errorf("fast endpoint address %q: endpoint name must not contain %q", raw, "/?#")
	}
	return name, nil
}

// fastRouteURL returns the canonical route URL for a urp endpoint name produced
// by parseFastAddr. String() yields "urp://<name>".
func fastRouteURL(name string) *url.URL {
	return &url.URL{Scheme: "urp", Host: name}
}

// fastAddrFromRouteURL returns the endpoint name of a fast route URL, or
// ok=false when u is not a fast route URL. It accepts the canonical form from
// fastRouteURL as well as the result of url.Parse on its String().
func fastAddrFromRouteURL(u *url.URL) (string, bool) {
	if u == nil || !strings.EqualFold(u.Scheme, "urp") {
		return _EMPTY_, false
	}
	if u.Host == _EMPTY_ {
		return _EMPTY_, false
	}
	if validateFastEndpointName(u.Host) != nil {
		return _EMPTY_, false
	}
	return u.Host, true
}

// isFastRouteURL reports whether u is a fast (urp://) route URL.
func isFastRouteURL(u *url.URL) bool {
	_, ok := fastAddrFromRouteURL(u)
	return ok
}

// errFastEndpointClosed is returned by a zeroCopyEndpoint once Close has run.
var errFastEndpointClosed = errors.New("fast endpoint closed")

// completionKind distinguishes the two owned-buffer completions a fast
// transport reports: a buffer that has received bytes, and a send buffer the
// device has finished transmitting (so it returns to FREE).
type completionKind uint8

const (
	completionRecv completionKind = iota // idx now holds received bytes (res = length)
	completionSend                       // idx finished transmitting (res = status)
)

// completion is one owned-buffer event demuxed from the transport's completion
// stream. It is transport-generic: the urp backend maps urpfast.Completion onto
// it (P2c), and a future TCP-io_uring backend (§9a) maps its own completions
// onto the same shape.
type completion struct {
	kind completionKind
	idx  uint32
	res  int32
}

// zeroCopyEndpoint is the seam every fast-transport backend satisfies. No urp /
// RDMA words appear here on purpose: the urp backend (*urpfast.Pool, §2) is one
// implementation, and the TCP-io_uring backend (§9a, IORING_OP_SEND_ZC +
// provided-buffer RX) is a sibling behind the same interface, so fastConn and
// the readLoop/writeLoop seams (§4/§5) are written once.
type zeroCopyEndpoint interface {
	// Recv hands the parser a pool-owned buffer view carrying one received
	// message; the caller re-arms that buffer for RX via ReRecv(idx) once it
	// has finished parsing (except while the buffer is held IN_FLIGHT for a
	// forward-from-recv TX, §4).
	Recv() (idx uint32, payload []byte, err error)
	ReRecv(idx uint32) error
	// Send transmits length bytes from owned buffer idx; the buffer stays
	// IN_FLIGHT until its completionSend arrives on Completions().
	Send(idx, length uint32) error
	// SendBuf hands back a FREE owned buffer for server-originated bytes
	// (protocol control that did not arrive in a recv buffer, §4).
	SendBuf() (idx uint32, buf []byte, err error)
	// BufSize is the fixed byte size of an owned send/recv buffer; the TX
	// seam plans how to split an outbound buffer larger than this across
	// several sends (planFastSend).
	BufSize() int
	// Completions is the demuxed OnRecv / OnSendComplete stream.
	Completions() <-chan completion
	// RetainBuffers / ReleaseBuffers bracket a consumer's use of the payload
	// memory Recv hands out, and they are part of this interface rather than an
	// optional capability because getting them wrong is a use-after-free, and a
	// backend that forgets them should fail to compile rather than fail to fault
	// (design 58 P2e-5o).
	//
	// Recv returns a slice INTO the backend's buffer pool. For a backend whose
	// pool is an mmap released at Close — both of ours — that memory is valid only
	// while someone holds a retain. Close runs on whichever goroutine noticed the
	// connection die, which is usually NOT the goroutine parsing a delivered
	// buffer, so without this the parser reads unmapped memory: observed as a
	// SIGSEGV in client.parse on a delivered RX buffer, with a page-aligned
	// pointer, not a Go panic.
	//
	// RetainBuffers reports false if the pool is already gone, which means the
	// caller must stop rather than proceed with memory it cannot be given. Pair
	// every true with exactly one ReleaseBuffers; Close and the retains are
	// unordered by design, because the consumer is frequently also the closer
	// (a parse error tears the connection down from inside the Recv/ReRecv
	// window), so a Close that waited for the reader would deadlock there. A
	// backend whose payloads are ordinary Go memory implements these as a true
	// and a no-op.
	RetainBuffers() bool
	ReleaseBuffers()
	// Close releases the endpoint; in-flight completions may still drain. It does
	// not necessarily release payload memory — see RetainBuffers.
	Close() error
}

// fastAddr is the net.Addr a fast endpoint reports so routeTransport-style
// reporting can name the transport without a net.Conn. Network() is "urp-fast";
// String() is the urp endpoint name.
type fastAddr struct{ name string }

func (a fastAddr) Network() string { return "urp-fast" }
func (a fastAddr) String() string  { return a.name }

// fastConn bridges a zeroCopyEndpoint to the client. It owns the endpoint and
// exposes the RX/TX primitives the readLoop/writeLoop seams call (P2b/P2c),
// plus RemoteAddr so route/transport reporting can name the endpoint. It is
// deliberately transport-agnostic: it never mentions urp.
type fastConn struct {
	ep    zeroCopyEndpoint
	raddr net.Addr

	// transport is the name this route reports (routeTransportFast for the urp
	// backend, routeTransportTCPZC for tcpzc). createRoute reads it so both
	// fc-backed transports are distinguishable in /varz.
	transport string

	// closedCh is closed exactly once, by Close, so the accept supervisor
	// (startFastRouteAccept) can block until this route tears down and then
	// re-arm the endpoint for the next peer attach (route model §8). Close is
	// reached on teardown via flushAndClose's fast-endpoint branch.
	closeOnce sync.Once
	closedCh  chan struct{}
}

// newFastConn wraps an endpoint bound to the urp endpoint name.
func newFastConn(ep zeroCopyEndpoint, name string) *fastConn {
	return newFastConnTransport(ep, name, routeTransportFast)
}

// newFastConnTransport wraps an endpoint and records the transport name it
// reports. The urp backend uses routeTransportFast; the tcpzc backend uses
// routeTransportTCPZC.
func newFastConnTransport(ep zeroCopyEndpoint, name, transport string) *fastConn {
	return &fastConn{ep: ep, raddr: fastAddr{name: name}, transport: transport, closedCh: make(chan struct{})}
}

// Closed returns a channel closed when the endpoint has been Closed.
func (fc *fastConn) Closed() <-chan struct{} { return fc.closedCh }

// RemoteAddr reports the fast endpoint address (a *net.UnixAddr-analog) so
// transport reporting can distinguish it from TCP/unix routes.
func (fc *fastConn) RemoteAddr() net.Addr { return fc.raddr }

// Recv / ReRecv / Send / SendBuf / Completions / Close forward to the endpoint;
// the seams (§4/§5) call these rather than the endpoint directly so the client
// only ever depends on fastConn, never on a concrete backend.
func (fc *fastConn) Recv() (uint32, []byte, error)    { return fc.ep.Recv() }
func (fc *fastConn) ReRecv(idx uint32) error          { return fc.ep.ReRecv(idx) }
func (fc *fastConn) Send(idx, length uint32) error    { return fc.ep.Send(idx, length) }
func (fc *fastConn) SendBuf() (uint32, []byte, error) { return fc.ep.SendBuf() }
func (fc *fastConn) BufSize() int                     { return fc.ep.BufSize() }
func (fc *fastConn) Completions() <-chan completion   { return fc.ep.Completions() }
func (fc *fastConn) RetainBuffers() bool              { return fc.ep.RetainBuffers() }
func (fc *fastConn) ReleaseBuffers()                  { fc.ep.ReleaseBuffers() }

// Close releases the endpoint and signals Closed() exactly once. It is
// idempotent: teardown may call it more than once (flushAndClose on the RX
// path, plus the accept supervisor on re-arm), so the close-channel guard uses
// sync.Once while the underlying endpoint Close is forwarded each time (backends
// make their own Close idempotent).
func (fc *fastConn) Close() error {
	fc.closeOnce.Do(func() { close(fc.closedCh) })
	return fc.ep.Close()
}

// fastSendSeg describes one urp Send derived from an outbound buffer: bytes
// [off:off+length] of the source buffer are transmitted in a single owned send
// buffer. When forward is true the source buffer is itself a recv-owned pool
// buffer eligible to be sent in place (zero copy, §4); otherwise the segment's
// bytes are copied into a FREE send buffer before Send. A buffer larger than
// the send-buffer size is split into consecutive copy segments.
type fastSendSeg struct {
	off     int
	length  int
	forward bool
}

// planFastSend maps one outbound buffer to the ordered sends needed to
// transmit it over a fast endpoint whose owned buffers are sendBufSize bytes.
//
// forwardFromRecv marks buf as a recv-owned buffer eligible for a zero-copy
// send in place (§4, produced by the P2c RX-forward path — never by the copies
// that queueOutbound places in c.out.nb). Such a buffer is forwarded whole only
// when it fits one send buffer; an oversize forward buffer cannot be split in
// place, so it degrades to the copy-segment path (correct by construction, the
// byte-stream floor). An empty buffer or a non-positive sendBufSize yields no
// segments.
func planFastSend(buf []byte, sendBufSize int, forwardFromRecv bool) []fastSendSeg {
	if sendBufSize <= 0 || len(buf) == 0 {
		return nil
	}
	if forwardFromRecv && len(buf) <= sendBufSize {
		return []fastSendSeg{{off: 0, length: len(buf), forward: true}}
	}
	segs := make([]fastSendSeg, 0, (len(buf)+sendBufSize-1)/sendBufSize)
	for off := 0; off < len(buf); off += sendBufSize {
		length := sendBufSize
		if off+length > len(buf) {
			length = len(buf) - off
		}
		segs = append(segs, fastSendSeg{off: off, length: length})
	}
	return segs
}

// flushOutboundFast is the fast-endpoint analog of flushOutbound (design 58
// P2b): it drains the pending outbound buffers (c.out.nb) over the zero-copy
// endpoint rather than a net.Conn. Every buffer in c.out.nb was already copied
// out of any recv buffer by queueOutbound, so each is transmitted via the
// copy path — a FREE owned send buffer per planFastSend segment, filled and
// handed to the device with Send. (The zero-copy forward-from-recv path, §4,
// originates in the P2c RX seam and never reaches c.out.nb, so forwardFromRecv
// is false here.) The client lock is held on entry, as with flushOutbound, and
// released around the device sends. Returns true when data was attempted so
// the caller need not re-queue a flush signal.
func (c *client) flushOutboundFast() bool {
	if c.flags.isSet(flushOutbound) {
		// Mirror flushOutbound: back off if a competing flush is in flight.
		c.mu.Unlock()
		runtime.Gosched()
		c.mu.Lock()
		return false
	}
	c.flags.set(flushOutbound)
	defer c.flags.clear(flushOutbound)

	// Nothing to do.
	if c.fast == nil || c.srv == nil || c.out.pb == 0 {
		return true
	}

	// Take the pending buffers; queueOutbound may append more while we send.
	nb := c.out.nb
	c.out.nb = nil
	sendBufSize := c.fast.BufSize()
	fast := c.fast

	// Do NOT hold the lock during device IO.
	c.mu.Unlock()

	var sent int64
	var flushErr error

	// Hold the endpoint's buffer pool across this unlocked send window (design 58
	// P2e-5o). SendBuf hands back a slice INTO that pool and the copy below writes
	// through it, so this seam is a pool holder exactly as readLoopFast is — a
	// concurrent Close that released the mapping here would fault on the write
	// rather than the read, but it is the same bug. A refused retain means the pool
	// is already gone, so there is nothing to send into: report it as a write error
	// and let the normal teardown below run, which still recycles the nbPool frames.
	retained := fast.RetainBuffers()
	if !retained {
		flushErr = errFastEndpointClosed
	} else {
		defer fast.ReleaseBuffers()
	}
	if retained {
	outer:
		for _, buf := range nb {
			for _, seg := range planFastSend(buf, sendBufSize, false) {
				idx, sb, err := fast.SendBuf()
				if err != nil {
					flushErr = err
					break outer
				}
				n := copy(sb, buf[seg.off:seg.off+seg.length])
				if err := fast.Send(idx, uint32(n)); err != nil {
					flushErr = err
					break outer
				}
				sent += int64(n)
			}
		}
	}

	// Re-acquire the client lock.
	c.mu.Lock()

	// Recycle the drained nbPool frames (the send bytes were copied into
	// owned buffers, so these are free to return regardless of send outcome).
	for i := range nb {
		nbPoolPut(nb[i])
	}

	c.out.pb -= sent
	if flushErr != nil {
		c.markConnAsClosed(WriteError)
		return true
	}
	// If queueOutbound appended more while we were unlocked, signal a follow-up.
	if c.out.pb > 0 {
		c.flushSignal()
	}
	return true
}

// readLoopFast is the fast-endpoint RX seam (design 58 P2c): the zero-copy
// analog of readLoop. Instead of reading bytes off a net.Conn into a resizing
// buffer, it pulls whole pool buffers by index from the endpoint (Recv), feeds
// each buffer's bytes to the existing streaming parser in place (no copy at the
// seam), and re-arms the buffer for the device (ReRecv) as soon as parse
// returns.
//
// Re-arming immediately is safe by construction (§6, the byte-stream floor):
// the NATS parser copies any bytes it must carry across a buffer boundary out
// of the input slice before parse returns — a split control line into c.argBuf
// (clonePubArg) and a split message payload into c.msgBuf/c.scratch — and a
// whole message that lands within one buffer is delivered synchronously
// (queueOutbound copies it into the outbound pool) and its slice cleared before
// return. So no live reference into the recv buffer survives the parse call,
// and the buffer can return to the device at once. (The forward-from-recv
// zero-copy TX path of §4, which would instead hold a recv buffer IN_FLIGHT
// until its send completes, is deferred; this copy-path seam never holds one.)
//
// The client lock is released for the duration of the loop, matching readLoop;
// s.grWG.Done() is handled by readLoop's deferred call, so it is not repeated
// here. This runs only for a fast client (c.fast != nil).
func (c *client) readLoopFast() {
	c.mu.Lock()
	s := c.srv
	if c.isClosed() {
		c.mu.Unlock()
		return
	}
	fast := c.fast
	acc := c.acc
	c.mu.Unlock()

	// Hold the endpoint's payload memory for as long as this loop can be looking
	// at it (design 58 P2e-5o). Every payload below is a slice into the backend's
	// buffer pool, and Close runs on whoever noticed the connection die — usually
	// another goroutine entirely — so without this retain the mapping can be
	// released while c.parse is reading one. A false means the pool is already
	// gone: there is nothing left to read, so leave instead of taking a buffer we
	// cannot be given. The release is deferred rather than paired with ReRecv
	// because every exit below (parse error, Recv error, re-arm failure) leaves
	// without re-arming, several of them from inside closeConnection.
	if !fast.RetainBuffers() {
		return
	}
	defer fast.ReleaseBuffers()

	// Non-websocket parse always uses a single buffer view per iteration.
	var _bufs [1][]byte
	bufs := _bufs[:1]

	for {
		idx, payload, err := fast.Recv()
		if err != nil {
			c.closeConnection(closedStateForErr(err))
			return
		}

		c.in.start = time.Now()

		// Clear inbound stats cache (mirrors readLoop).
		c.in.msgs = 0
		c.in.bytes = 0
		c.in.subs = 0

		bufs[0] = payload

		// Main call into the parser for inbound data. Identical error handling
		// to readLoop's non-websocket path.
		if perr := c.parse(bufs[0]); perr != nil {
			if perr == ErrMinimumVersionRequired {
				return
			}
			if dur := time.Since(c.in.start); dur >= readLoopReportThreshold {
				c.Warnf("Readloop processing time: %v", dur)
			}
			c.flushClients(0)
			if perr != ErrMaxPayload && perr != ErrAuthentication && perr != ErrConnectionClosed {
				c.Error(perr)
				c.closeConnection(ProtocolViolation)
			}
			return
		}
		c.resetReadLoopStallTime()

		// Re-arm the recv buffer for the device. Safe now (see doc above): the
		// parser has copied out anything it carries past this buffer.
		if rerr := fast.ReRecv(idx); rerr != nil {
			c.closeConnection(closedStateForErr(rerr))
			return
		}

		// Update stats collected while parsing (mirrors readLoop).
		if c.in.msgs > 0 {
			inMsgs := int64(c.in.msgs)
			inBytes := int64(c.in.bytes)

			atomic.AddInt64(&c.inMsgs, inMsgs)
			atomic.AddInt64(&c.inBytes, inBytes)

			if acc != nil {
				acc.stats.Lock()
				acc.stats.inMsgs += inMsgs
				acc.stats.inBytes += inBytes
				if c.kind == LEAF {
					acc.stats.ln.inMsgs += inMsgs
					acc.stats.ln.inBytes += inBytes
				}
				acc.stats.Unlock()
			}

			if c.kind == CLIENT {
				atomic.AddInt64(&s.inClientMsgs, inMsgs)
				atomic.AddInt64(&s.inClientBytes, inBytes)
			}

			atomic.AddInt64(&s.inMsgs, inMsgs)
			atomic.AddInt64(&s.inBytes, inBytes)
		}

		// Signal writeLoop to flush any deliveries this parse produced.
		last := c.flushClients(0)

		c.mu.Lock()
		if c.in.msgs > 0 || c.in.subs > 0 {
			c.last = last
			c.lastIn = last
		}
		// re-snapshot the account since it can change during reload, etc.
		acc = c.acc
		closed := c.isClosed()
		c.mu.Unlock()

		if closed {
			return
		}
	}
}

// fastEndpointOpener constructs a real zeroCopyEndpoint attached to the named
// urp endpoint (opening /dev/urp + REGISTER). It is nil until a device backend
// is linked into the binary — the urp backend from tools/urp-fast-go, which is
// wired in P2d once that module is pushed + vendored (the fork cannot import it
// before then). Keeping the backend behind this hook lets the entire fast route
// layer build and be unit-tested in the fork now (tests install a mock opener),
// and defers only the device open to P2d. A backend registers itself from an
// init()/build-tagged file via setFastEndpointOpener.
var fastEndpointOpener func(name string) (zeroCopyEndpoint, error)

// setFastEndpointOpener installs the device backend used by openFastEndpoint.
// The urp backend calls this at link time; tests call it to inject a mock.
func setFastEndpointOpener(open func(name string) (zeroCopyEndpoint, error)) {
	fastEndpointOpener = open
}

// openFastEndpoint attaches to the pre-provisioned urp endpoint named name and
// returns the fastConn a route is built over. It errors when no device backend
// is linked (the default fork build) so a fast route surfaces a clear failure
// rather than a nil-deref; a real backend is present only once the urp module
// is vendored in (P2d).
func (s *Server) openFastEndpoint(name string) (*fastConn, error) {
	if fastEndpointOpener == nil {
		return nil, fmt.Errorf("fast endpoint %q: no urp backend linked into this build", name)
	}
	ep, err := fastEndpointOpener(name)
	if err != nil {
		return nil, fmt.Errorf("fast endpoint %q: %w", name, err)
	}
	return newFastConn(ep, name), nil
}

// fastTCPZCOpener constructs a zeroCopyEndpoint over an already-connected TCP
// socket fd, driving it with io_uring IORING_OP_SEND_ZC + recv (the tcpzc
// backend, tools/urp-fast-go/tcpzc, design 58 §9a). Unlike fastEndpointOpener it
// takes a connected fd rather than a urp endpoint name — NATS dials/accepts the
// TCP socket itself (the tcp/uds route template) and hands ownership here — so it
// is a separate seam. nil until the linux tcpzc backend is linked (a backend
// registers itself from fast_tcpzc_linux.go's init via setFastTCPZCOpener).
var fastTCPZCOpener func(fd int, name string) (zeroCopyEndpoint, error)

// setFastTCPZCOpener installs the tcpzc backend used by openTCPZCEndpoint. The
// linux tcpzc backend calls this at link time; tests may inject a mock.
func setFastTCPZCOpener(open func(fd int, name string) (zeroCopyEndpoint, error)) {
	fastTCPZCOpener = open
}

// openTCPZCEndpoint wraps an owned, connected TCP socket fd in a fastConn driven
// by the tcpzc io_uring backend. The route layer dials/accepts the socket, hands
// ownership of fd here (the backend closes it on Close), and builds a route over
// the returned fastConn exactly as the urp fast path does. It errors when no
// tcpzc backend is linked (non-linux, or a build without it).
func (s *Server) openTCPZCEndpoint(fd int, name string) (*fastConn, error) {
	if fastTCPZCOpener == nil {
		return nil, fmt.Errorf("tcpzc endpoint %q: no tcpzc backend linked into this build", name)
	}
	ep, err := fastTCPZCOpener(fd, name)
	if err != nil {
		return nil, fmt.Errorf("tcpzc endpoint %q: %w", name, err)
	}
	return newFastConnTransport(ep, name, routeTransportTCPZC), nil
}

// fastTCPZCOwnFd extracts an owned, blocking socket fd from a dialed/accepted
// route net.Conn for the tcpzc backend (it dups the descriptor out of the
// net.Conn so io_uring, not Go's runtime poller, drives it, then closes the
// original). nil unless the linux tcpzc backend is linked; the backend registers
// its ownFdFromConn from fast_tcpzc_linux.go's init via setFastTCPZCOwnFd.
var fastTCPZCOwnFd func(nc net.Conn) (int, error)

// setFastTCPZCOwnFd installs the tcpzc fd-extraction helper. The linux tcpzc
// backend calls this at link time; tests may inject a mock.
func setFastTCPZCOwnFd(own func(nc net.Conn) (int, error)) {
	fastTCPZCOwnFd = own
}

// openTCPZCConn takes ownership of a connected route socket (the route layer just
// dialed or accepted it as a plain TCP net.Conn) and wraps it in a tcpzc
// fastConn. It dups the fd out of nc (so io_uring owns it) and closes nc, then
// hands the fd to the tcpzc backend, which closes it on Close (and on any open
// error). On an extraction failure nc is closed here. It errors when no tcpzc
// backend is linked (non-linux, or a build without it), so a misconfigured tcpzc
// cluster surfaces a clear failure rather than a nil-deref.
func (s *Server) openTCPZCConn(nc net.Conn, name string) (*fastConn, error) {
	if fastTCPZCOwnFd == nil || fastTCPZCOpener == nil {
		_ = nc.Close()
		return nil, fmt.Errorf("tcpzc route %q: no tcpzc backend linked into this build", name)
	}
	fd, err := fastTCPZCOwnFd(nc)
	if err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("tcpzc route %q: %w", name, err)
	}
	// ownFdFromConn has already closed nc on success; fd is ours now and is
	// closed by the tcpzc backend (openTCPZCEndpoint -> tcpzc.Open owns it on
	// every path).
	return s.openTCPZCEndpoint(fd, name)
}

// connectFastRoute solicits a fast (urp://) route: it attaches to the named urp
// endpoint and builds the route over it. An Explicit route retries until the
// endpoint is attachable (the peer's `urp add` may not be ready yet) or the
// server stops; the reconnect-on-drop path (reConnectToRoute -> connectToRoute)
// re-enters here. It runs inside connectToRoute's goroutine, so it does NOT
// touch s.grWG (design 58 route model §7/§8).
func (s *Server) connectFastRoute(rURL *url.URL, rtype RouteType, firstConnect bool, gossipMode byte, accName string) {
	name, ok := fastAddrFromRouteURL(rURL)
	if !ok {
		s.Errorf("Invalid fast route URL %q", rURL.Redacted())
		return
	}
	tryForEver := rtype == Explicit
	opts := s.getOpts()

	attemptDelay := routeConnectDelay
	reconnectTimer := time.NewTimer(attemptDelay)
	reconnectTimer.Stop()
	defer stopAndClearTimer(&reconnectTimer)

	for attempts := 0; s.isRunning(); {
		if tryForEver && !s.routeStillValid(rURL) {
			s.Debugf("Not attempting to attach to explicit fast route %q, no longer configured", rURL.Redacted())
			return
		}
		fc, err := s.openFastEndpoint(name)
		if err != nil {
			attempts++
			if s.shouldReportConnectErr(firstConnect, attempts) {
				s.Errorf("Error attaching to fast route %q (attempt %v): %v", rURL.Redacted(), attempts, err)
			} else {
				s.Debugf("Error attaching to fast route %q (attempt %v): %v", rURL.Redacted(), attempts, err)
			}
			if !tryForEver {
				if opts.Cluster.ConnectRetries <= 0 || attempts > opts.Cluster.ConnectRetries {
					return
				}
			}
			reconnectTimer.Reset(attemptDelay)
			select {
			case <-s.quitCh:
				return
			case <-reconnectTimer.C:
				if opts.Cluster.ConnectBackoff {
					attemptDelay *= 2
					if attemptDelay > routeConnectMaxDelay {
						attemptDelay = routeConnectMaxDelay
					}
				}
				continue
			}
		}
		if tryForEver && !s.routeStillValid(rURL) {
			fc.Close()
			return
		}
		s.createRoute(nil, fc, rURL, rtype, gossipMode, accName)
		return
	}
}

// startFastRouteAccept is the fast-endpoint accept arm (design 58 route model
// §7/§8): the analog of the Route accept loop for a connectionless endpoint. A
// fast endpoint carries exactly one peer attach, so rather than an
// accept-connections loop it opens the endpoint, builds an Implicit route, and
// blocks until that route tears down (fastConn.Closed) before re-arming for the
// next attach. Launched via startGoRoutine, so it owns one s.grWG token.
func (s *Server) startFastRouteAccept(name string) {
	defer s.grWG.Done()
	for s.isRunning() {
		fc, err := s.openFastEndpoint(name)
		if err != nil {
			s.Errorf("Error opening fast route endpoint %q: %v", name, err)
			select {
			case <-s.quitCh:
				return
			case <-time.After(routeConnectDelay):
				continue
			}
		}
		c := s.createRoute(nil, fc, nil, Implicit, gossipDefault, _EMPTY_)
		if c == nil {
			fc.Close()
			select {
			case <-s.quitCh:
				return
			case <-time.After(routeConnectDelay):
				continue
			}
		}
		// Wait for this route to close, then re-arm for the next peer attach.
		select {
		case <-s.quitCh:
			return
		case <-fc.Closed():
		}
	}
}
