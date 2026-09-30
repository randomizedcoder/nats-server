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

// Design 58 P2e — the tcpzc device-free backend behind the fast route transport.
// It is the sibling of fast_urp_linux.go: same zeroCopyEndpoint contract and the
// same single-ring-owner concurrency model, but it drives an ordinary connected
// TCP socket with io_uring IORING_OP_SEND_ZC (real zero-copy TX above a size
// threshold) + io_uring recv (recv-syscall/alloc elimination) instead of urp
// uring_cmds against /dev/urp. It carries no RDMA and no urp module, so it
// benefits any TCP deployment (design 58 §9a, the upstream-friendly track).
//
// Unlike the urp backend it takes a CONNECTED fd, not an endpoint name: NATS
// dials/accepts the TCP socket itself (the tcp/uds route template) and hands
// ownership of the raw fd to openTCPZCEndpointFd, which is installed via
// setFastTCPZCOpener at init. tcp/uds/fast deployments are unaffected (this seam
// is only reached for a tcpzc route).
//
// Concurrency: a single goroutine (ioLoop) owns the ring; the client's
// read/write loops hand buffer indices over channels. Backpressure falls out of
// the fixed-size TX free-buffer channel exactly as in the urp backend — but here
// the socket's own send buffer (TCP flow control) is the ultimate backpressure,
// so there is no QP to drive into an error state under overload.

import (
	"context"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/randomizedcoder/uds-rdma-proxy/tools/urp-fast-go/tcpzc"
)

func init() {
	setFastTCPZCOpener(openTCPZCEndpointFd)
	setFastTCPZCOwnFd(ownFdFromConn)
}

// tcpzc pool geometry defaults. Env-overridable so the deploy can align them with
// the peer. The pool is split in half: the low half are RX landing slots, the
// high half are the FREE pool SendBuf hands out for TX. There is no reserved wire
// header (a TCP byte stream carries the NATS protocol raw), so the whole buffer
// is usable payload.
const (
	tcpzcDefaultBufSize uint32 = 65536
	tcpzcDefaultCount   uint32 = 256
	tcpzcPollTimeout           = 200 * time.Microsecond
)

// tcpzcEndpoint is the zeroCopyEndpoint over a tcpzc.Conn. See the file header
// for the single-ring-owner concurrency model. It mirrors urpEndpoint; the
// differences are the fd-based open and the SEND_ZC two-CQE completion handling
// (a send result with CQEFMore holds the buffer until its notification).
type tcpzcEndpoint struct {
	conn   *tcpzc.Conn
	usable int

	recvCh   chan recvEvent  // ioLoop -> Recv
	reRecvCh chan uint32     // ReRecv -> ioLoop (re-arm an RX buffer)
	sendCh   chan sendReq    // Send -> ioLoop (post a TX buffer)
	txFreeCh chan uint32     // FREE TX buffer indices: SendBuf pops, ioLoop pushes
	comps    chan completion // send-completion observation (interface contract)

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// openTCPZCEndpointFd builds a tcpzc.Conn over the connected socket fd, arms the
// RX half, and starts the ring goroutine. It is the func installed via
// setFastTCPZCOpener. name is used only for error/reporting context. On any
// failure it closes fd (via conn.Close) so the caller never double-owns it.
func openTCPZCEndpointFd(fd int, name string) (zeroCopyEndpoint, error) {
	bufSize := envUint32("NATS_TCPZC_BUF_SIZE", tcpzcDefaultBufSize)
	count := envUint32("NATS_TCPZC_BUF_COUNT", tcpzcDefaultCount)
	if count < 4 {
		count = 4
	}

	// tcpzc.Open takes ownership of fd and closes it on ANY error, so there is no
	// fd to reclaim here on failure.
	conn, err := tcpzc.Open(fd, tcpzc.Config{
		BufSize:            bufSize,
		Count:              count,
		SmallSendThreshold: envUint32("NATS_TCPZC_ZC_THRESHOLD", tcpzc.DefaultSmallSendThreshold),
	})
	if err != nil {
		return nil, err
	}

	rxN := tcpzcRXCount(count)
	e := &tcpzcEndpoint{
		conn:     conn,
		usable:   int(conn.Usable()),
		recvCh:   make(chan recvEvent, rxN),
		reRecvCh: make(chan uint32, rxN),
		sendCh:   make(chan sendReq, count-rxN),
		txFreeCh: make(chan uint32, count-rxN),
		comps:    make(chan completion, count),
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())

	// Seed the TX free pool with the high half.
	for i := rxN; i < count; i++ {
		e.txFreeCh <- i
	}
	// Arm the RX half as recv landing slots.
	for i := uint32(0); i < rxN; i++ {
		if perr := conn.PostRecv(i); perr != nil {
			_ = conn.Close()
			return nil, perr
		}
	}

	e.wg.Add(1)
	go e.ioLoop()
	return e, nil
}

// tcpzcRXCount splits count buffers into RX landing slots (half) and the TX free
// pool (the rest), clamped so both halves are at least one buffer. Mirrors
// urpRXCount.
func tcpzcRXCount(count uint32) uint32 {
	rx := count / 2
	if rx < 1 {
		rx = 1
	}
	if rx >= count {
		rx = count - 1
	}
	return rx
}

// ioLoop owns the ring: drain producer intents (post SENDs / re-arm RECVs), then
// poll + reap, then dispatch each completion. Exits on ctx cancel (Close) or a
// torn-down ring.
func (e *tcpzcEndpoint) ioLoop() {
	defer e.wg.Done()
	dst := make([]tcpzc.Completion, e.conn.Entries())
	for {
		select {
		case <-e.ctx.Done():
			return
		default:
		}
		e.drainIntents()
		n, err := e.conn.PollOnce(dst, tcpzcPollTimeout)
		if err != nil {
			e.closeOnce.Do(e.cancel)
			return
		}
		for i := 0; i < n; i++ {
			e.dispatch(dst[i])
		}
	}
}

// drainIntents posts every queued Send / ReRecv without blocking. Runs on the
// ring goroutine, so these are the only PostSend/PostRecv callers after open.
func (e *tcpzcEndpoint) drainIntents() {
	for {
		select {
		case req := <-e.sendCh:
			if serr := e.conn.PostSend(req.idx, req.length); serr != nil {
				e.conn.Free(req.idx)
				select {
				case e.txFreeCh <- req.idx:
				default:
				}
			}
		case idx := <-e.reRecvCh:
			_ = e.conn.PostRecv(idx)
		default:
			return
		}
	}
}

// dispatch applies one reaped completion per tcpzc.Classify. The SEND_ZC send
// result (ActSendInFlight) holds the buffer until its notification; only a
// terminal send completion (plain CQE or the notification) frees it.
func (e *tcpzcEndpoint) dispatch(c tcpzc.Completion) {
	act, length := tcpzc.Classify(c.Kind, c.Flags, c.Res, e.conn.BufSize())
	switch act {
	case tcpzc.ActFreeSend:
		_ = e.conn.Complete(c.Idx) // Send -> Free
		select {
		case e.txFreeCh <- c.Idx:
		default:
		}
		select {
		case e.comps <- completion{kind: completionSend, idx: c.Idx, res: c.Res}:
		default:
		}
	case tcpzc.ActSendInFlight:
		// SEND_ZC send acknowledged; buffer still held until the notification.
	case tcpzc.ActDeliverRecv:
		e.conn.Free(c.Idx) // Recv -> Free; re-armed on ReRecv
		e.deliver(recvEvent{idx: c.Idx, payload: e.conn.Buf(c.Idx)[:length]})
	case tcpzc.ActRecvError:
		e.conn.Free(c.Idx)
		e.deliver(recvEvent{err: errFastEndpointClosed})
	case tcpzc.ActIgnore:
		// forged/unknown kind — drop.
	}
}

func (e *tcpzcEndpoint) deliver(ev recvEvent) {
	select {
	case e.recvCh <- ev:
	case <-e.ctx.Done():
	}
}

// Recv blocks for the next received message (or terminal error).
func (e *tcpzcEndpoint) Recv() (uint32, []byte, error) {
	select {
	case ev := <-e.recvCh:
		return ev.idx, ev.payload, ev.err
	case <-e.ctx.Done():
		return 0, nil, errFastEndpointClosed
	}
}

// ReRecv re-arms RX buffer idx once the parser has consumed it.
func (e *tcpzcEndpoint) ReRecv(idx uint32) error {
	select {
	case e.reRecvCh <- idx:
		return nil
	case <-e.ctx.Done():
		return errFastEndpointClosed
	}
}

// Send queues buffer idx (already filled with length payload bytes) for
// transmission; the buffer returns to the FREE pool on its send completion (the
// SEND_ZC notification, or a plain send's single CQE).
func (e *tcpzcEndpoint) Send(idx, length uint32) error {
	select {
	case e.sendCh <- sendReq{idx: idx, length: length}:
		return nil
	case <-e.ctx.Done():
		return errFastEndpointClosed
	}
}

// SendBuf hands back a FREE owned buffer for server-originated bytes, blocking
// for TX backpressure until one is available (or the endpoint closes).
func (e *tcpzcEndpoint) SendBuf() (uint32, []byte, error) {
	select {
	case idx := <-e.txFreeCh:
		return idx, e.conn.Buf(idx)[:e.usable], nil
	case <-e.ctx.Done():
		return 0, nil, errFastEndpointClosed
	}
}

// BufSize is the usable payload capacity of one owned buffer.
func (e *tcpzcEndpoint) BufSize() int { return e.usable }

// Completions is the send-completion observation stream (drained by tests; the
// production seams do not require it).
func (e *tcpzcEndpoint) Completions() <-chan completion { return e.comps }

// Close cancels the ring goroutine, waits for it to exit, then releases the conn
// (which closes the socket fd). Idempotent.
func (e *tcpzcEndpoint) Close() error {
	e.closeOnce.Do(e.cancel)
	e.wg.Wait()
	return e.conn.Close()
}

// ownFdFromConn extracts an owned socket fd from a dialed/accepted net.Conn,
// suitable for handing to tcpzc.Open (which drives it via io_uring, NOT Go's
// netpoller). It dups the descriptor so the returned fd survives closing nc, then
// closes nc to release the original from the runtime poller. The caller (via
// tcpzc.Conn.Close) owns and closes the returned fd.
//
// The dup shares the connection's open-file-description, so it inherits the
// O_NONBLOCK flag Go sets on net sockets — the returned fd is non-blocking. That
// is correct (and preferred) for io_uring: a send/recv that would block is
// retried internally via io_uring's poll machinery rather than parked on an io-wq
// worker, so the CQE still completes when the socket is ready. We deliberately do
// NOT clear O_NONBLOCK.
func ownFdFromConn(nc net.Conn) (int, error) {
	sc, ok := nc.(syscall.Conn)
	if !ok {
		return -1, errFastEndpointClosed
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var dup int
	var dupErr error
	if cerr := raw.Control(func(fd uintptr) {
		dup, dupErr = syscall.Dup(int(fd))
	}); cerr != nil {
		return -1, cerr
	}
	if dupErr != nil {
		return -1, dupErr
	}
	// Release the original fd from Go's runtime poller; the dup we own is
	// independent and stays open for io_uring.
	_ = nc.Close()
	return dup, nil
}
