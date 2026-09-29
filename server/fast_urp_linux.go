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

// Design 58 P2d — the urp device backend behind the fast route transport. This
// file is the only place NATS touches /dev/urp: it implements the (backend-
// agnostic) zeroCopyEndpoint interface over a urpfast.Pool and installs itself
// via setFastEndpointOpener at init. It is Linux-only and self-registering, so
// a build that includes it gains a fast backend automatically; a build without
// it (or a non-Linux build) leaves fastEndpointOpener nil and openFastEndpoint
// reports "no urp backend linked" — tcp/uds deployments are unaffected either
// way (openFastEndpoint is only ever called for a fast route).
//
// Concurrency model: a single goroutine (ioLoop) owns the io_uring ring. The
// client's read/write loops (Recv/ReRecv/Send/SendBuf) never touch the ring;
// they hand buffer indices to ioLoop over channels, so every SQE production,
// submit, wait and reap happens on one goroutine. This sidesteps any
// producer/consumer ring-sharing subtlety at the cost of one channel hop per
// operation. Backpressure falls out of the fixed-size channels: SendBuf blocks
// when the TX pool is exhausted (until a send completes and frees a buffer),
// which stalls the write loop exactly as a full socket buffer would.

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	urpfast "github.com/randomizedcoder/uds-rdma-proxy/tools/urp-fast-go"
	"github.com/randomizedcoder/uds-rdma-proxy/tools/urp-fast-go/fastcore"
)

func init() { setFastEndpointOpener(openURPEndpoint) }

// Pool geometry defaults. Env-overridable so the deploy (the NixOS module /
// harness) can align them with the endpoint's `urp add --kind fast
// --buffer-size/--buffer-count` provisioning. The pool is split in half: the
// low half are RX landing slots (cycled by Recv/ReRecv), the high half are the
// FREE pool SendBuf hands out for TX.
const (
	urpDefaultBufSize uint32 = 65536 // 64 KiB per buffer (payload usable = bufSize-HeaderResv)
	urpDefaultCount   uint32 = 256   // 128 RX + 128 TX; ~16 MiB pinned at 64 KiB
	urpPollTimeout           = 200 * time.Microsecond
)

// urpAction is ioLoop's decision for one reaped completion. Kept as a pure
// classification (classifyURPCompletion) so it is table-testable without a ring.
type urpAction int

const (
	actIgnore      urpAction = iota // unknown user_data kind — drop
	actDeliverRecv                  // RX buffer holds `length` payload bytes
	actRecvError                    // RX completion failed — tear the endpoint down
	actFreeSend                     // TX buffer finished — return it to the FREE pool
)

// classifyURPCompletion maps a reaped completion (its kind and kernel result)
// to the action ioLoop takes, validating a RECV's landed length against the
// usable buffer capacity (RxAccept, the -EOVERFLOW guard). Pure: no ring, no
// channels, no state — exhaustively table-tested (POS/NEG/BND/COR).
func classifyURPCompletion(kind uint8, res int32, bufSize uint32) (act urpAction, length uint32) {
	switch kind {
	case fastcore.UDKindSend:
		return actFreeSend, 0
	case fastcore.UDKindRecv:
		if res < 0 {
			return actRecvError, 0
		}
		n, err := fastcore.RxAccept(res, bufSize)
		if err != nil {
			return actRecvError, 0
		}
		return actDeliverRecv, n
	default:
		return actIgnore, 0
	}
}

// urpRXCount returns how many of count pool buffers are RX landing slots (the
// rest are the TX free pool). Half and half, clamped so both halves are at
// least one buffer.
func urpRXCount(count uint32) uint32 {
	rx := count / 2
	if rx < 1 {
		rx = 1
	}
	if rx >= count {
		rx = count - 1
	}
	return rx
}

// recvEvent carries one received message (or a terminal error) from ioLoop to
// Recv. payload aliases the pinned pool buffer idx and is valid until the caller
// re-arms it with ReRecv.
type recvEvent struct {
	idx     uint32
	payload []byte
	err     error
}

type sendReq struct{ idx, length uint32 }

// urpEndpoint is the zeroCopyEndpoint over a urpfast.Pool. See the file header
// for the single-ring-owner concurrency model.
type urpEndpoint struct {
	pool   *urpfast.Pool
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

// openURPEndpoint opens /dev/urp, REGISTERs a pinned pool against the named urp
// fast endpoint, arms the RX half, and starts the ring goroutine. It is the
// func installed via setFastEndpointOpener.
func openURPEndpoint(name string) (zeroCopyEndpoint, error) {
	bufSize := envUint32("URP_NATS_BUF_SIZE", urpDefaultBufSize)
	count := envUint32("URP_NATS_BUF_COUNT", urpDefaultCount)
	if count < 4 {
		count = 4
	}

	pool, err := urpfast.Open(urpfast.Config{
		Endpoint: name,
		BufSize:  bufSize,
		Count:    count,
		MsgSize:  bufSize - fastcore.HeaderResv, // arm the full usable region for RX
	})
	if err != nil {
		return nil, err
	}

	rxN := urpRXCount(count)
	txN := count - rxN
	e := &urpEndpoint{
		pool:     pool,
		usable:   int(pool.Usable()),
		recvCh:   make(chan recvEvent, rxN),
		reRecvCh: make(chan uint32, rxN),
		sendCh:   make(chan sendReq, txN),
		txFreeCh: make(chan uint32, txN),
		comps:    make(chan completion, count),
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())

	// Seed the TX free pool with the high half.
	for i := rxN; i < count; i++ {
		e.txFreeCh <- i
	}
	// Arm the RX half as RECV landing slots (submitted by ioLoop's first poll).
	for i := uint32(0); i < rxN; i++ {
		if perr := pool.PostRecv(i); perr != nil {
			_ = pool.Close()
			return nil, perr
		}
	}

	e.wg.Add(1)
	go e.ioLoop()
	return e, nil
}

// ioLoop owns the ring: drain producer intents (post SENDs / re-arm RECVs),
// then submit + wait + reap, then dispatch each completion. Exits on ctx
// cancel (Close) or a torn-down ring (PollOnce error).
func (e *urpEndpoint) ioLoop() {
	defer e.wg.Done()
	dst := make([]urpfast.Completion, e.pool.Entries())
	for {
		select {
		case <-e.ctx.Done():
			return
		default:
		}
		e.drainIntents()
		n, err := e.pool.PollOnce(dst, urpPollTimeout)
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
// ring goroutine, so these are the only PostSend/PostRecv callers after Open.
func (e *urpEndpoint) drainIntents() {
	for {
		select {
		case req := <-e.sendCh:
			if serr := e.pool.PostSend(req.idx, req.length); serr != nil {
				// Could not arm the send: reclaim the buffer so SendBuf can reuse
				// it rather than leaking it out of the TX pool.
				e.pool.Free(req.idx)
				select {
				case e.txFreeCh <- req.idx:
				default:
				}
			}
		case idx := <-e.reRecvCh:
			// Best-effort re-arm; a failure drops this buffer from the RX
			// rotation (the endpoint degrades but stays correct).
			_ = e.pool.PostRecv(idx)
		default:
			return
		}
	}
}

// dispatch applies one reaped completion: recycle a finished send, or hand a
// received buffer to Recv.
func (e *urpEndpoint) dispatch(c urpfast.Completion) {
	act, length := classifyURPCompletion(c.Kind, c.Res, e.pool.BufSize())
	switch act {
	case actFreeSend:
		_ = e.pool.Complete(c.Idx) // Send -> Free
		select {
		case e.txFreeCh <- c.Idx:
		default: // cap == txN, so this never actually blocks/drops in practice
		}
		select {
		case e.comps <- completion{kind: completionSend, idx: c.Idx, res: c.Res}:
		default: // nobody is required to drain Completions(); never block ioLoop
		}
	case actDeliverRecv:
		e.pool.Free(c.Idx) // Recv -> Free; re-armed on ReRecv
		e.deliver(recvEvent{idx: c.Idx, payload: e.pool.Buf(c.Idx)[:length]})
	case actRecvError:
		e.pool.Free(c.Idx)
		e.deliver(recvEvent{err: errFastEndpointClosed})
	case actIgnore:
		// forged/unknown kind — drop.
	}
}

// deliver enqueues a recv event, unblocking if the endpoint is closing.
func (e *urpEndpoint) deliver(ev recvEvent) {
	select {
	case e.recvCh <- ev:
	case <-e.ctx.Done():
	}
}

// Recv blocks for the next received message (or terminal error).
func (e *urpEndpoint) Recv() (uint32, []byte, error) {
	select {
	case ev := <-e.recvCh:
		return ev.idx, ev.payload, ev.err
	case <-e.ctx.Done():
		return 0, nil, errFastEndpointClosed
	}
}

// ReRecv re-arms RX buffer idx for the device once the parser has consumed it.
func (e *urpEndpoint) ReRecv(idx uint32) error {
	select {
	case e.reRecvCh <- idx:
		return nil
	case <-e.ctx.Done():
		return errFastEndpointClosed
	}
}

// Send queues buffer idx (already filled with `length` payload bytes) for
// transmission; the buffer returns to the FREE pool on its send completion.
func (e *urpEndpoint) Send(idx, length uint32) error {
	select {
	case e.sendCh <- sendReq{idx: idx, length: length}:
		return nil
	case <-e.ctx.Done():
		return errFastEndpointClosed
	}
}

// SendBuf hands back a FREE owned buffer for server-originated bytes, blocking
// for TX backpressure until one is available (or the endpoint closes).
func (e *urpEndpoint) SendBuf() (uint32, []byte, error) {
	select {
	case idx := <-e.txFreeCh:
		return idx, e.pool.Buf(idx)[:e.usable], nil
	case <-e.ctx.Done():
		return 0, nil, errFastEndpointClosed
	}
}

// BufSize is the usable payload capacity of one owned buffer (the TX seam uses
// it to split an oversize outbound buffer across sends).
func (e *urpEndpoint) BufSize() int { return e.usable }

// Completions is the send-completion observation stream (drained by tests; the
// production seams do not require it, so ioLoop only ever offers non-blocking).
func (e *urpEndpoint) Completions() <-chan completion { return e.comps }

// Close cancels the ring goroutine, waits for it to exit, then releases the
// pool. Idempotent: the accept supervisor and the RX teardown may both call it.
func (e *urpEndpoint) Close() error {
	e.closeOnce.Do(e.cancel)
	e.wg.Wait()
	return e.pool.Close()
}

// envUint32 reads a positive uint32 from the environment, falling back to def.
func envUint32(key string, def uint32) uint32 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil && n > 0 {
			return uint32(n)
		}
	}
	return def
}
