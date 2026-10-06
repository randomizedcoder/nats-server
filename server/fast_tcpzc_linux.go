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
	"fmt"
	"io"
	"net"
	"os"
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
	// tcpzcDefaultPollUS is the ioLoop's io_uring_enter wait window.
	//
	// It was documented here as "a SAFETY NET, not the latency path: an inbound
	// completion wakes the enter immediately", on the reasoning that a longer window
	// costs little latency while cutting the idle wakeup rate proportionally. That
	// reasoning was measured and it is wrong in BOTH halves (design 58 P2e-5l/5m):
	// raising the window 25x (200µs -> 5ms) barely moved the enter rate at all
	// (1,294,105 -> 1,245,657 per node), because at waitNr=1 the window is hardly
	// ever reached. Every completion, and every local Send/ReRecv through the wake
	// flag, ends the wait at once -- so the loop spins at the message rate and never
	// accumulates a batch. Measured on hardware: 0.19-0.27 messages per enter, about
	// five enters PER message.
	//
	// The window only becomes the latency path once the loop actually waits, which is
	// what NATS_TCPZC_WAIT_NR turns on. Sweep the two together; neither default moves
	// without a hardware A/B. Override with NATS_TCPZC_POLL_US.
	tcpzcDefaultPollUS uint32 = 200
)

// tcpzcWaitNrFromEnv resolves how many completions one io_uring_enter waits for,
// from NATS_TCPZC_WAIT_NR. The default is tcpzc.DefaultWaitNr (1), which is
// byte-identical to every tcpzc measurement taken before design 58 P2e-5m: the
// knob exists to be swept on hardware, not to change behaviour on arrival.
//
// Above 1 the loop genuinely waits for a batch, bounded by the poll window, and the
// window stops being an idle safety net and becomes the latency budget -- see
// tcpzcDefaultPollUS and tcpzc.DefaultWaitNr. The library clamps the value to what
// the ring can satisfy (tcpzc.WaitNrFor), so an over-large setting degrades to the
// ring depth rather than hanging on a wait that can never complete.
func tcpzcWaitNrFromEnv() uint32 {
	return envUint32("NATS_TCPZC_WAIT_NR", tcpzc.DefaultWaitNr)
}

// tcpzcPollTimeoutFromEnv resolves the ioLoop's wait window from
// NATS_TCPZC_POLL_US. envUint32 already rejects a non-numeric or zero value, and
// zero must stay rejected here: PollOnce treats timeout <= 0 as "non-blocking",
// which makes the loop a pure userspace spin that yields only via Gosched -- the
// shape measured at 6.3ms ping-pong RTT, and the opposite of what someone lowering
// this knob is asking for.
func tcpzcPollTimeoutFromEnv() time.Duration {
	return time.Duration(envUint32("NATS_TCPZC_POLL_US", tcpzcDefaultPollUS)) * time.Microsecond
}

// tcpzcEndpoint is the zeroCopyEndpoint over a tcpzc.Conn. See the file header
// for the single-ring-owner concurrency model. It mirrors urpEndpoint; the
// differences are the fd-based open and the SEND_ZC two-CQE completion handling
// (a send result with CQEFMore holds the buffer until its notification).
type tcpzcEndpoint struct {
	conn   *tcpzc.Conn
	usable int
	pollTO time.Duration
	waitNr uint32 // completions one enter waits for; see tcpzcWaitNrFromEnv
	name   string // route name, so a debug dump identifies WHICH ring is hot

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

	// Split: buffers [0,rxN) land recvs in the provided-buffer ring, [rxN,count)
	// are the TX free pool. Use the library's own split so the TX pool can never
	// disagree with the set of buffers the ring owns.
	rxN := tcpzc.RXCountFor(count)

	// tcpzc.Open takes ownership of fd and closes it on ANY error, so there is no
	// fd to reclaim here on failure. Open also publishes buffers [0,RXCount) into
	// a provided-buffer ring and arms ONE multishot recv over it, so the RX half
	// is armed in bulk by the library and consumed in ring order (design 58 P2e-5
	// — the per-buffer PostRecv arming this replaces was unordered and shredded
	// the byte stream, §P2e-3-HW).
	conn, err := tcpzc.Open(fd, tcpzc.Config{
		BufSize:            bufSize,
		Count:              count,
		RXCount:            rxN,
		SmallSendThreshold: envUint32("NATS_TCPZC_ZC_THRESHOLD", tcpzc.DefaultSmallSendThreshold),
	})
	if err != nil {
		return nil, err
	}

	e := &tcpzcEndpoint{
		conn:     conn,
		usable:   int(conn.Usable()),
		pollTO:   tcpzcPollTimeoutFromEnv(),
		waitNr:   tcpzcWaitNrFromEnv(),
		name:     name,
		recvCh:   make(chan recvEvent, rxN),
		reRecvCh: make(chan uint32, rxN),
		sendCh:   make(chan sendReq, count-rxN),
		txFreeCh: make(chan uint32, count-rxN),
		comps:    make(chan completion, count),
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())

	// Seed the TX free pool with the high half. (The RX half is already published
	// to the provided-buffer ring and armed by tcpzc.Open.)
	for i := rxN; i < count; i++ {
		e.txFreeCh <- i
	}

	e.wg.Add(1)
	go e.ioLoop()
	return e, nil
}

// ioLoop owns the ring: drain producer intents (post SENDs / re-arm RECVs), then
// poll + reap, then dispatch each completion. Exits on ctx cancel (Close) or a
// torn-down ring.
func (e *tcpzcEndpoint) ioLoop() {
	defer e.wg.Done()
	dst := make([]tcpzc.Completion, e.conn.Entries())
	var dbg tcpzcDebugState
	for {
		select {
		case <-e.ctx.Done():
			return
		default:
		}
		e.drainIntents()
		n, err := e.conn.PollBatch(dst, e.waitNr, e.pollTO)
		if err != nil {
			e.closeOnce.Do(e.cancel)
			return
		}
		for i := 0; i < n; i++ {
			e.dispatch(dst[i])
		}
		if tcpzcDebug {
			e.debugTick(&dbg)
		}
	}
}

// tcpzcDebug enables the P2e-5l per-ring counter dump on stderr (and so into the
// unit's journal, which is where the harness can grep it). Read once at startup:
// the check is on the ioLoop's hottest path, and a spinning loop runs it ~1M
// times/s, so it must be a plain bool test and nothing more.
//
// Why stderr and not /routez: the zeroCopyEndpoint seam deliberately carries no
// logger and no server reference (that is what makes it transport-neutral and
// unit-testable), and plumbing one through is a seam redesign that has been
// deferred since P2e-5g. Two separate re-arm spins have now been found only by
// reading a CPU profile after the fact, so the diagnosis is worth more than the
// aesthetics of its delivery. The dump is off unless NATS_TCPZC_DEBUG=1.
var tcpzcDebug = os.Getenv("NATS_TCPZC_DEBUG") == "1"

// tcpzcDebugInterval is how often a ring reports. One second matches the
// per-second rates every other tcpzc measurement is quoted in.
//
// tcpzcDebugIterMask is how many ioLoop iterations pass between clock reads
// (2^k-1, tested with &). A spinning loop iterates ~1M times/s and time.Now() is
// ~25ns, so reading the clock every iteration would itself cost ~2.5% of the core
// being measured. Every 4096 iterations bounds that at a rounding error while
// still reporting ~245x per second at spin rates, and at least once per interval
// at idle (an idle loop still wakes every pollTO).
//
// tcpzcDebugOut is where a line goes. All three are vars, not consts, ONLY so a
// test can drive a real ring and assert that a line actually lands: the first
// attempt to validate this on hardware lost a run, and a diagnostic whose output
// path is unproven is worth nothing (that is the whole lesson of P2e-5k).
var (
	tcpzcDebugInterval           = time.Second
	tcpzcDebugIterMask           = 4095
	tcpzcDebugOut      io.Writer = os.Stderr
)

// tcpzcDebugState is one ioLoop's dump bookkeeping. Owned by that goroutine.
type tcpzcDebugState struct {
	iter int
	next time.Time
	prev tcpzc.Stats
}

// debugTick emits one line per ring per interval:
//
//	tcpzc-dbg name=<route> spin=<verdict> enters=N msgs=N enobufs=N armed=N refused=N ...
//
// `spin` is tcpzc.DiagnoseSpin over the interval delta — the whole point, because
// it names the mechanism rather than leaving a wall of numbers to be eyeballed.
// The floor is one tenth of the ~1.07M enters/s/node a real spin measured on
// hardware, which is still two orders of magnitude above a healthy route.
func (e *tcpzcEndpoint) debugTick(d *tcpzcDebugState) {
	d.iter++
	if d.iter&tcpzcDebugIterMask != 0 {
		return
	}
	now := time.Now()
	if d.next.IsZero() {
		// First visit: start the interval, do not report a delta against a zero
		// snapshot (it would include everything since Open and read as a spike).
		d.next = now.Add(tcpzcDebugInterval)
		d.prev = e.conn.Stats()
		return
	}
	if now.Before(d.next) {
		return
	}
	cur := e.conn.Stats()
	delta := cur.Sub(d.prev)
	elapsed := tcpzcDebugInterval + now.Sub(d.next)
	fmt.Fprint(tcpzcDebugOut, tcpzcDebugLine(e.name, delta, elapsed))
	d.prev = cur
	d.next = now.Add(tcpzcDebugInterval)
}

// tcpzcDebugSpinFloorPerSec is the io_uring_enter rate above which an interval is
// worth attributing to a mechanism. One tenth of the ~1.07M/s/node a real spin
// measured on hardware, which is still two orders of magnitude above a healthy
// route (the clean 3-node tcpzc arm ran ~65k/s/node across all 8 of its rings).
const tcpzcDebugSpinFloorPerSec = 100000

// tcpzcDebugLine formats one dump line. Pure, so the format and the verdict
// selection are table-testable without a ring: the interesting field is `spin`,
// which names the mechanism instead of leaving a wall of numbers to be eyeballed
// — the thing that was missing when P2e-5k had to be reasoned out by hand.
//
// The TX fields (design 58 P2e-5n) answer the other question the dump could not:
// `zcSends`/`zcBytes` against `txSends`/`txBytes` is whether IORING_OP_SEND_ZC ran
// at all over the interval, and `zcPctBytes` is the share of bytes that took it.
// Bytes, not sends, are what zero copy saves, so the byte share is the headline
// and the send counts are there to show how differently the two can read.
// `notifs` must track `zcSends`: each SEND_ZC owes exactly one notification and
// that notification is what releases the buffer, so a standing gap is TX buffers
// pinned forever.
func tcpzcDebugLine(name string, delta tcpzc.Stats, elapsed time.Duration) string {
	floor := uint64(tcpzcDebugSpinFloorPerSec) * uint64(elapsed) / uint64(time.Second)
	txSends := delta.SendsPlain + delta.SendsZC
	txBytes := delta.TxBytesPlain + delta.TxBytesZC
	zcPct := 0.0
	if txBytes > 0 {
		zcPct = 100 * float64(delta.TxBytesZC) / float64(txBytes)
	}
	return fmt.Sprintf(
		"tcpzc-dbg name=%s spin=%s ms=%d enters=%d msgs=%d enobufs=%d armed=%d refused=%d armEnded=%d returned=%d ringAvail=%d isArmed=%v"+
			" txSends=%d zcSends=%d txBytes=%d zcBytes=%d notifs=%d zcPctBytes=%.1f\n",
		name, tcpzc.DiagnoseSpin(delta, floor), elapsed.Milliseconds(),
		delta.Syscalls, delta.RecvDelivered, delta.RecvENOBUFS, delta.ArmSubmitted,
		delta.ArmRefused, delta.ArmEnded, delta.BufsReturned, delta.RingAvail, delta.RecvArmed,
		txSends, delta.SendsZC, txBytes, delta.TxBytesZC, delta.NotifsReaped, zcPct)
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
			// Republish to the provided-buffer ring: a memory write + tail
			// advance, no SQE and no syscall (the RX batching win).
			_ = e.conn.ReturnRecvBuf(idx)
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

	// Checked BEFORE the action: a send that wrote fewer bytes than were posted
	// dropped the rest, so the route stream now has a hole and the peer's parser is
	// misaligned. MSG_WAITALL makes this unreachable, but carrying on would corrupt
	// silently -- tear the route down instead and let NATS reconnect.
	if e.conn.SendWasShort(c) {
		e.conn.Free(c.Idx)
		select {
		case e.txFreeCh <- c.Idx:
		default:
		}
		e.deliver(recvEvent{err: errFastEndpointClosed})
		return
	}

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
	case tcpzc.ActSendCanceled:
		// An earlier send in this IOSQE_IO_LINK ordering chain failed, so this
		// one never ran. Reclaim its buffer; the first failure drives teardown.
		e.conn.Free(c.Idx)
		select {
		case e.txFreeCh <- c.Idx:
		default:
		}
	case tcpzc.ActDeliverRecv:
		e.conn.Free(c.Idx) // app owns it now; ReRecv republishes it to the ring
		e.deliver(recvEvent{idx: c.Idx, payload: e.conn.Buf(c.Idx)[:length]})
	case tcpzc.ActRecvRearm:
		// The multishot arm ended without a stream error (e.g. -ENOBUFS because
		// the ring momentarily held no buffer). No buffer was delivered and no
		// bytes were lost — just arm a fresh multishot.
	case tcpzc.ActRecvError:
		e.conn.Free(c.Idx)
		e.deliver(recvEvent{err: errFastEndpointClosed})
	case tcpzc.ActIgnore:
		// forged/unknown kind — drop.
	}

	// Checked INDEPENDENTLY of the action: the final completion of a multishot
	// arm can both deliver bytes and terminate the arm, so a delivering CQE with
	// CQEFMore clear still needs a re-arm or RX stops forever.
	if act != tcpzc.ActRecvError && tcpzc.RecvEndedArm(c.Kind, c.Flags) {
		if aerr := e.conn.ArmRecvMultishot(); aerr != nil {
			e.deliver(recvEvent{err: aerr})
		}
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
		// Pull ioLoop out of a blocking io_uring_enter: a channel send cannot
		// interrupt one, so without this the handoff waits out the poll window and
		// the poll timeout becomes the latency floor (design 58 P2e-5c).
		e.conn.Wake()
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
		e.conn.Wake() // see ReRecv: the handoff must interrupt a blocked poll
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

// RetainBuffers / ReleaseBuffers forward the buffer-pool lifetime contract to the
// Conn: Recv hands out slices of its mmap'd pool, which must outlive the last
// holder rather than the owner. See zeroCopyEndpoint and tcpzc.Conn.poolRefs.
func (e *tcpzcEndpoint) RetainBuffers() bool { return e.conn.PoolRef() }
func (e *tcpzcEndpoint) ReleaseBuffers()     { e.conn.PoolUnref() }

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
