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

package server

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

// mockEndpoint is an in-memory loopback zeroCopyEndpoint for exercising the
// fastConn seam without a device: SendBuf hands out a FREE buffer, Send copies
// its bytes into a FREE buffer that Recv then returns (modelling the wire), and
// each Send is reported as a completionSend (the send buffer returns to FREE)
// followed by a completionRecv (bytes arrived). It carries no urp/RDMA
// dependency, so it also proves the transport-generic seam compiles standalone.
type mockEndpoint struct {
	mu      sync.Mutex
	bufs    [][]byte
	bufSize int
	free    []uint32
	lens    map[uint32]int
	recvQ   []uint32
	comps   chan completion
	closed  bool
}

func newMockEndpoint(count, bufSize int) *mockEndpoint {
	m := &mockEndpoint{
		bufs:    make([][]byte, count),
		bufSize: bufSize,
		free:    make([]uint32, 0, count),
		lens:    make(map[uint32]int, count),
		comps:   make(chan completion, count*2),
	}
	for i := 0; i < count; i++ {
		m.bufs[i] = make([]byte, bufSize)
		m.free = append(m.free, uint32(i))
	}
	return m
}

func (m *mockEndpoint) BufSize() int { return m.bufSize }

func (m *mockEndpoint) takeFreeLocked() (uint32, bool) {
	if len(m.free) == 0 {
		return 0, false
	}
	idx := m.free[len(m.free)-1]
	m.free = m.free[:len(m.free)-1]
	return idx, true
}

func (m *mockEndpoint) SendBuf() (uint32, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, nil, errFastEndpointClosed
	}
	idx, ok := m.takeFreeLocked()
	if !ok {
		return 0, nil, errFastEndpointClosed
	}
	return idx, m.bufs[idx], nil
}

func (m *mockEndpoint) Send(idx, length uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errFastEndpointClosed
	}
	// Loopback: copy the sent bytes into a FREE recv buffer.
	rIdx, ok := m.takeFreeLocked()
	if !ok {
		return errFastEndpointClosed
	}
	copy(m.bufs[rIdx], m.bufs[idx][:length])
	m.lens[rIdx] = int(length)
	m.recvQ = append(m.recvQ, rIdx)
	// The send buffer returns to FREE once the device is done reading it.
	m.free = append(m.free, idx)
	m.comps <- completion{kind: completionSend, idx: idx, res: int32(length)}
	m.comps <- completion{kind: completionRecv, idx: rIdx, res: int32(length)}
	return nil
}

func (m *mockEndpoint) Recv() (uint32, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.recvQ) == 0 {
		return 0, nil, errFastEndpointClosed
	}
	idx := m.recvQ[0]
	m.recvQ = m.recvQ[1:]
	return idx, m.bufs[idx][:m.lens[idx]], nil
}

func (m *mockEndpoint) ReRecv(idx uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.free = append(m.free, idx)
	return nil
}

func (m *mockEndpoint) Completions() <-chan completion { return m.comps }

func (m *mockEndpoint) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.comps)
	}
	return nil
}

// TestFastConnLoopback proves the fastConn seam round-trips a payload over a
// loopback endpoint: SendBuf -> fill -> Send yields a send-complete plus a
// recv completion, and Recv returns the same bytes. This is the P2a loopback
// proof; the readLoop/writeLoop wiring that consumes it lands in P2b/P2c.
func TestFastConnLoopback(t *testing.T) {
	ep := newMockEndpoint(8, 256)
	fc := newFastConn(ep, "nats-a")
	defer fc.Close()

	payload := []byte("PING hello.world\r\n")

	idx, buf, err := fc.SendBuf()
	if err != nil {
		t.Fatalf("SendBuf: %v", err)
	}
	n := copy(buf, payload)
	if err := fc.Send(idx, uint32(n)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Two completions: send-complete for idx, then recv for the loopback buffer.
	comps := fc.Completions()
	var sawSend bool
	var recvIdx uint32
	for i := 0; i < 2; i++ {
		c := <-comps
		switch c.kind {
		case completionSend:
			if c.idx != idx {
				t.Fatalf("send completion idx = %d, expected %d", c.idx, idx)
			}
			if int(c.res) != n {
				t.Fatalf("send completion res = %d, expected %d", c.res, n)
			}
			sawSend = true
		case completionRecv:
			recvIdx = c.idx
			if int(c.res) != n {
				t.Fatalf("recv completion res = %d, expected %d", c.res, n)
			}
		}
	}
	if !sawSend {
		t.Fatal("expected a send completion")
	}

	gotIdx, got, err := fc.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if gotIdx != recvIdx {
		t.Fatalf("Recv idx = %d, expected %d", gotIdx, recvIdx)
	}
	if string(got) != string(payload) {
		t.Fatalf("Recv payload = %q, expected %q", got, payload)
	}
	if err := fc.ReRecv(gotIdx); err != nil {
		t.Fatalf("ReRecv: %v", err)
	}
}

// TestFastConnRemoteAddr asserts the fast endpoint reports a distinct net.Addr
// so route/transport reporting can name it apart from tcp/unix.
func TestFastConnRemoteAddr(t *testing.T) {
	fc := newFastConn(newMockEndpoint(1, 64), "nats-a")
	defer fc.Close()
	addr := fc.RemoteAddr()
	if addr.Network() != "urp-fast" {
		t.Fatalf("Network() = %q, expected %q", addr.Network(), "urp-fast")
	}
	if addr.String() != "nats-a" {
		t.Fatalf("String() = %q, expected %q", addr.String(), "nats-a")
	}
	// It must not masquerade as a *net.UnixAddr (routeTransport would misreport).
	if _, ok := addr.(*net.UnixAddr); ok {
		t.Fatal("fast addr must not be a *net.UnixAddr")
	}
}

// TestFastCompletionDemux asserts fastConn forwards the endpoint's completion
// stream faithfully by kind and index (the demux the RX/TX seams rely on). The
// out-of-range / forged-id / ring-reuse correlation is proven upstream in the
// driver library's DecodeCompletion table (tools/urp-fast-go); here we assert
// the fork-local seam neither drops nor reorders what the backend delivers.
func TestFastCompletionDemux(t *testing.T) {
	for _, tc := range []struct {
		description string
		seq         []completion
	}{
		{
			description: "in-order send then recv",
			seq:         []completion{{kind: completionSend, idx: 3, res: 10}, {kind: completionRecv, idx: 4, res: 10}},
		},
		{
			description: "interleaved recvs and sends",
			seq: []completion{
				{kind: completionRecv, idx: 0, res: 1}, {kind: completionSend, idx: 1, res: 2},
				{kind: completionRecv, idx: 2, res: 3}, {kind: completionSend, idx: 0, res: 4},
			},
		},
		{
			description: "single recv",
			seq:         []completion{{kind: completionRecv, idx: 7, res: 64}},
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			// Size the completion buffer to the sequence so the pre-load
			// below never blocks (pool buffers are unused here).
			ep := newMockEndpoint(len(tc.seq), 8)
			fc := newFastConn(ep, "nats-a")
			for _, c := range tc.seq {
				ep.comps <- c
			}
			out := fc.Completions()
			for i, want := range tc.seq {
				got := <-out
				if got != want {
					t.Fatalf("completion %d = %+v, expected %+v", i, got, want)
				}
			}
		})
	}
}

// TestPlanFastSend covers the TX buffer-selection / segmentation planner
// (design 58 P2b). Rows are POS/NEG/BND/COR; each names the input buffer, the
// send-buffer size, whether the buffer is a forward-from-recv candidate, and
// the expected segments. The byte-stream floor each row degrades to (copy
// segments) is asserted whenever forwarding does not apply.
func TestPlanFastSend(t *testing.T) {
	for _, tc := range []struct {
		description string
		bufLen      int
		sendBufSize int
		forward     bool
		expected    []fastSendSeg
	}{
		// positive
		{
			description: "server-originated copy fits one send buffer",
			bufLen:      100, sendBufSize: 256, forward: false,
			expected: []fastSendSeg{{off: 0, length: 100}},
		},
		{
			description: "forward-from-recv fits one send buffer (zero copy)",
			bufLen:      100, sendBufSize: 256, forward: true,
			expected: []fastSendSeg{{off: 0, length: 100, forward: true}},
		},
		// boundary
		{
			description: "payload exactly fills one send buffer (copy)",
			bufLen:      256, sendBufSize: 256, forward: false,
			expected: []fastSendSeg{{off: 0, length: 256}},
		},
		{
			description: "forward buffer exactly fills one send buffer",
			bufLen:      256, sendBufSize: 256, forward: true,
			expected: []fastSendSeg{{off: 0, length: 256, forward: true}},
		},
		{
			description: "one byte over a send buffer splits into two (copy)",
			bufLen:      257, sendBufSize: 256, forward: false,
			expected: []fastSendSeg{{off: 0, length: 256}, {off: 256, length: 1}},
		},
		// corner
		{
			description: "spans several send buffers (copy)",
			bufLen:      600, sendBufSize: 256, forward: false,
			expected: []fastSendSeg{{off: 0, length: 256}, {off: 256, length: 256}, {off: 512, length: 88}},
		},
		{
			description: "oversize forward buffer degrades to copy segments",
			bufLen:      600, sendBufSize: 256, forward: true,
			expected: []fastSendSeg{{off: 0, length: 256}, {off: 256, length: 256}, {off: 512, length: 88}},
		},
		// negative
		{
			description: "empty buffer yields no segments",
			bufLen:      0, sendBufSize: 256, forward: false,
			expected: nil,
		},
		{
			description: "non-positive send-buffer size yields no segments",
			bufLen:      100, sendBufSize: 0, forward: false,
			expected: nil,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			buf := make([]byte, tc.bufLen)
			got := planFastSend(buf, tc.sendBufSize, tc.forward)
			if len(got) != len(tc.expected) {
				t.Fatalf("segments = %+v, expected %+v", got, tc.expected)
			}
			var total int
			for i := range got {
				if got[i] != tc.expected[i] {
					t.Fatalf("segment %d = %+v, expected %+v", i, got[i], tc.expected[i])
				}
				total += got[i].length
			}
			// Every non-empty plan must cover the whole buffer exactly once
			// (the byte-stream floor: no bytes dropped, none duplicated).
			if tc.sendBufSize > 0 && total != tc.bufLen {
				t.Fatalf("segments cover %d bytes, expected %d", total, tc.bufLen)
			}
		})
	}
}

// TestFastRecvFraming proves the RX seam's byte-stream floor (design 58 P2c
// §6): feeding the parser a NATS stream split at fast-endpoint buffer
// boundaries produces the same result as one contiguous feed, for every
// boundary. Each row is a stream and a set of split sizes; the row parses
// cleanly (err == nil) and ends on a message boundary (OP_START) regardless of
// where the buffers cut it — including a payload straddling two buffers, a
// header split from its payload, packed messages, an empty payload and a
// control-only frame. This is exactly what readLoopFast does: hand each recv
// buffer's bytes to c.parse in order. (Route RMSG split-buffer handling is
// covered exhaustively in parser_test.go; the concern here is only that
// chunking at buffer edges is equivalent to a single feed, a parser-state
// property independent of the op, so PUB is used for a clean, self-contained
// harness.)
func TestFastRecvFraming(t *testing.T) {
	for _, tc := range []struct {
		description string
		stream      []byte
		bufSize     int // fast recv buffer size the stream is chunked into
	}{
		// positive
		{
			description: "one message in one frame",
			stream:      []byte("PUB foo 5\r\nhello\r\n"),
			bufSize:     256,
		},
		{
			description: "multiple messages packed, chunked small",
			stream:      []byte("PUB a 1\r\nx\r\nPUB b 2\r\nyy\r\nPUB c 3\r\nzzz\r\n"),
			bufSize:     8,
		},
		// boundary
		{
			description: "payload straddles two frames",
			stream:      []byte("PUB foo 11\r\nhello world\r\n"),
			bufSize:     14, // cut mid-payload
		},
		{
			description: "header line split from payload at frame edge",
			stream:      []byte("PUB subject 5\r\nabcde\r\n"),
			bufSize:     15, // "PUB subject 5\r\n" then payload
		},
		{
			description: "single-byte frames force every state split",
			stream:      []byte("PUB foo 5\r\nhello\r\n"),
			bufSize:     1,
		},
		// corner
		{
			description: "zero-length payload",
			stream:      []byte("PUB foo 0\r\n\r\n"),
			bufSize:     4,
		},
		{
			description: "control-only frame (PING) then a message",
			stream:      []byte("PING\r\nPUB foo 2\r\nhi\r\n"),
			bufSize:     3,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			// Reference: one contiguous feed.
			ref := dummyClient()
			if err := ref.parse(tc.stream); err != nil {
				t.Fatalf("contiguous parse: unexpected err %v", err)
			}
			if ref.state != OP_START {
				t.Fatalf("contiguous parse ended in state %d, expected OP_START", ref.state)
			}

			// Chunked feed, mirroring readLoopFast: one c.parse per recv buffer.
			c := dummyClient()
			for off := 0; off < len(tc.stream); off += tc.bufSize {
				end := off + tc.bufSize
				if end > len(tc.stream) {
					end = len(tc.stream)
				}
				if err := c.parse(tc.stream[off:end]); err != nil {
					t.Fatalf("chunk [%d:%d]: unexpected err %v", off, end, err)
				}
			}
			if c.state != OP_START {
				t.Fatalf("chunked parse ended in state %d, expected OP_START (byte-stream floor violated)", c.state)
			}
			// Both feeds must land on the same clean boundary — the chunk sizes
			// must not change the parse outcome.
			if c.state != ref.state {
				t.Fatalf("chunked state %d != contiguous state %d", c.state, ref.state)
			}
		})
	}
}

// recordingEndpoint is a zeroCopyEndpoint whose recv side is pre-loaded with a
// byte stream split into fixed-size buffers (modelling what the device delivers
// to readLoopFast), and which records how many buffers were re-armed (ReRecv)
// and whether Close ran. Recv returns errFastEndpointClosed once the pre-loaded
// buffers are drained, so readLoopFast terminates through its normal
// close-on-recv-error path. The TX methods satisfy the interface but are unused
// by the RX-seam test.
type recordingEndpoint struct {
	mu      sync.Mutex
	bufSize int
	recvQ   [][]byte
	loaded  int
	reArmed int
	closed  bool
	comps   chan completion
}

func newRecordingEndpoint(count, bufSize int) *recordingEndpoint {
	return &recordingEndpoint{bufSize: bufSize, comps: make(chan completion, count*2)}
}

// loadStream splits stream into bufSize-byte recv buffers, mirroring how a
// byte stream arrives spread across device buffers.
func (m *recordingEndpoint) loadStream(stream []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for off := 0; off < len(stream); off += m.bufSize {
		end := off + m.bufSize
		if end > len(stream) {
			end = len(stream)
		}
		buf := make([]byte, end-off)
		copy(buf, stream[off:end])
		m.recvQ = append(m.recvQ, buf)
		m.loaded++
	}
}

func (m *recordingEndpoint) pendingRecvCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loaded
}

func (m *recordingEndpoint) reRecvCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reArmed
}

func (m *recordingEndpoint) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *recordingEndpoint) BufSize() int { return m.bufSize }

func (m *recordingEndpoint) Recv() (uint32, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.recvQ) == 0 {
		return 0, nil, errFastEndpointClosed
	}
	idx := uint32(m.loaded - len(m.recvQ))
	buf := m.recvQ[0]
	m.recvQ = m.recvQ[1:]
	return idx, buf, nil
}

func (m *recordingEndpoint) ReRecv(idx uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reArmed++
	return nil
}

func (m *recordingEndpoint) Send(idx, length uint32) error    { return nil }
func (m *recordingEndpoint) SendBuf() (uint32, []byte, error) { return 0, nil, errFastEndpointClosed }
func (m *recordingEndpoint) Completions() <-chan completion   { return m.comps }

func (m *recordingEndpoint) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.comps)
	}
	return nil
}

// TestReadLoopFast proves the RX-seam driver drains every recv buffer, feeds it
// to the parser, and re-arms it for the device (ReRecv), then tears the fast
// endpoint down cleanly when the stream ends. It is the P2c integration proof,
// the sibling of TestFlushOutboundFast for the read side: a NATS stream is
// pre-loaded onto a loopback endpoint split across several recv buffers, and
// after readLoopFast returns every consumed buffer has been re-armed and the
// endpoint closed.
func TestReadLoopFast(t *testing.T) {
	opts := defaultServerOptions
	s := New(&opts)

	ep := newRecordingEndpoint(16, 8)
	// Two whole messages, chunked into 8-byte recv buffers on the wire.
	ep.loadStream([]byte("PUB foo 5\r\nhello\r\nPUB bar 3\r\nbye\r\n"))
	nBufs := ep.pendingRecvCount()

	c := &client{kind: CLIENT, fast: newFastConn(ep, "nats-a")}
	c.srv = s
	c.mpay = -1
	c.msubs = -1
	c.mcl = MAX_CONTROL_LINE_SIZE
	c.registerWithAccount(s.globalAccount())

	// readLoopFast runs until Recv reports the stream drained, then closes.
	readLoopFastDone := make(chan struct{})
	go func() {
		c.readLoopFast()
		close(readLoopFastDone)
	}()

	select {
	case <-readLoopFastDone:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoopFast did not return after the stream drained")
	}

	if got := ep.reRecvCount(); got != nBufs {
		t.Fatalf("re-armed %d recv buffers, expected %d (one per consumed buffer)", got, nBufs)
	}
	if !ep.isClosed() {
		t.Fatal("readLoopFast did not close the fast endpoint on teardown")
	}
	// The parser must have advanced past the last message boundary.
	if c.state != OP_START {
		t.Fatalf("parser ended in state %d, expected OP_START", c.state)
	}
}

// TestFlushOutboundFast proves the TX seam drains c.out.nb over a fast endpoint
// and the bytes arrive intact and in order, including a payload that spans more
// than one send buffer. This is the P2b copy-path integration proof; the
// readLoop/writeLoop wiring that drives it on a live route lands in P2c.
func TestFlushOutboundFast(t *testing.T) {
	for _, tc := range []struct {
		description string
		payload     []byte
	}{
		{description: "fits one send buffer", payload: bytes.Repeat([]byte("x"), 100)},
		{description: "spans two send buffers", payload: bytes.Repeat([]byte("y"), 300)},
		{description: "exactly one send buffer", payload: bytes.Repeat([]byte("z"), 256)},
	} {
		t.Run(tc.description, func(t *testing.T) {
			ep := newMockEndpoint(16, 256)
			c := &client{kind: ROUTER, fast: newFastConn(ep, "nats-a")}
			c.srv = &Server{}

			c.mu.Lock()
			c.queueOutbound(tc.payload)
			if c.out.pb != int64(len(tc.payload)) {
				c.mu.Unlock()
				t.Fatalf("pending bytes = %d, expected %d", c.out.pb, len(tc.payload))
			}
			ok := c.flushOutboundFast()
			pb := c.out.pb
			c.mu.Unlock()

			if !ok {
				t.Fatal("flushOutboundFast returned false")
			}
			if pb != 0 {
				t.Fatalf("pending bytes after flush = %d, expected 0", pb)
			}

			// Reassemble everything that reached the loopback wire.
			var got []byte
			for {
				_, payload, err := ep.Recv()
				if err != nil {
					break
				}
				got = append(got, payload...)
			}
			if !bytes.Equal(got, tc.payload) {
				t.Fatalf("received %d bytes, expected %d (equal=%v)", len(got), len(tc.payload), bytes.Equal(got, tc.payload))
			}
		})
	}
}
