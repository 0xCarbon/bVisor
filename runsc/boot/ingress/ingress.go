// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ingress implements the Sentry-side endpoint of the Oca
// host-ingress data path (Oca #541), served over the AF_UNIX stream
// connection donated to runsc as --ingress-fd.
//
// The HOST owns the listener lifecycle and announces every host-accepted
// connection as a framed OPEN carrying the guest dial target; this relay
// dials that target inside the sandbox netstack (gonet loopback; see
// runsc/boot) and pumps framed bytes both ways.
//
// Frame: uint32 big-endian body length, then body:
//
//	byte 0       protocol version (Version)
//	byte 1       kind (one of Kind*)
//	rest         per-kind payload:
//
//	  OPEN         host→sentry: connID(4) target IP(16, v4-in-v6) port(2)
//	  DIAL_RESULT  sentry→host: connID(4) status(1)
//	  DATA         both:        connID(4) payload
//	  CLOSE_WRITE  both:        connID(4)  — half-close (no more data this way)
//	  CLOSE        both:        connID(4)  — full teardown of the connection
//
// The wire format mirrors the oca host relay
// (github.com/0xCarbon/oca/network/ingress.go) byte-for-byte; the leading
// version byte makes any drift fail closed, and the golden vectors in
// ingress_test.go pin the frozen bytes.
//
// Backpressure is transitive blocking with bounded per-connection queues:
// a slow guest fills its queue, the mux blocks, the socketpair fills, and
// the host stops reading its TCP connection — no unbounded buffering
// anywhere on the path.
//
// Teardown invariant: per-connection queues are NEVER closed. CLOSE rides
// an in-order frame to the pump, and relay teardown signals r.done and
// closes guest connections instead — so a channel close can never race the
// mux's send (no send-on-closed-panic), and the mux is free to park on a
// full queue. A parked mux periodically polls the donated conn for
// POLLRDHUP (peer close, observable even behind unread frames) so that a
// host teardown on a stalled connection still ends the relay: the sandbox
// never holds ambiguous state.
//
// Fail-closed: any framing, version, or protocol error ends Serve with an
// error and tears down every carried connection (closing the donated conn);
// the host observes the teardown on its end. A clean conn close (host
// teardown at checkpoint/stop, with restore re-donating a fresh FD) ends
// Serve with nil.
package ingress

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Wire protocol constants (frozen — see the package comment).
const (
	// Version is the wire version both sides encode as the first body byte.
	Version uint8 = 1

	KindOpen       uint8 = 1 // host→sentry: new accepted conn
	KindDialResult uint8 = 2 // sentry→host: guest dial outcome
	KindData       uint8 = 3 // payload, either direction
	KindCloseWrite uint8 = 4 // half-close, either direction
	KindClose      uint8 = 5 // full close, either direction

	// Dial statuses carried in DIAL_RESULT (KindDialResult).
	DialOK      uint8 = 1
	DialRefused uint8 = 2

	// fixedHeaderLen is the body length of the largest fixed-size frame
	// (OPEN): version(1) + kind(1) + connID(4) + IP(16) + port(2).
	fixedHeaderLen = 1 + 1 + 4 + 16 + 2

	// MinFrameBody is the smallest legal body: version + kind + connID.
	// Per-kind length validation happens after decode.
	MinFrameBody = 6

	// MaxDataChunk caps a single DATA payload. Larger host reads are split
	// across frames; the cap bounds frame allocation from a corrupt length
	// prefix.
	MaxDataChunk = 64 * 1024

	// MaxFrameBody caps any frame body so a hostile or corrupt length
	// prefix cannot force an unbounded allocation.
	MaxFrameBody = fixedHeaderLen + MaxDataChunk

	// queueDepth bounds the per-connection frame queue. When full, the mux
	// blocks — that is the backpressure contract.
	queueDepth = 16

	// peerCheckInterval bounds how long a mux parked on a full queue runs
	// without re-probing the donated conn for host teardown.
	peerCheckInterval = 250 * time.Millisecond
)

// ErrRelayClosed reports the relay is already torn down; operations racing
// the teardown end without touching the wire.
var ErrRelayClosed = errors.New("ingress relay closed")

// Dialer dials one guest-side connection for a host-announced target. The
// target IP arrives in the 16-byte v4-in-v6 wire form. Production wires this
// to a gonet dialer over the sandbox netstack (runsc/boot), which unmaps
// v4-in-v6 and bounds the dial; tests wire it to a local listener.
type Dialer func(target [16]byte, port uint16) (net.Conn, error)

// Relay is the Sentry-side endpoint of the donated ingress conn. It owns
// frame encoding (serialized writes), the reader/mux loop, and
// per-connection dispatch queues. Create with NewRelay and run Serve.
type Relay struct {
	conn net.Conn
	dial Dialer

	writeMu  sync.Mutex // serializes frame writes
	doneOnce sync.Once
	done     chan struct{} // closed when the relay is torn down

	mu     sync.Mutex
	conns  map[uint32]*carried
	guests map[uint32]net.Conn
}

// carried is one connection's mux state: the bounded queue of frames
// awaiting the guest-side pump. The queue is never closed; teardown is
// signalled through Relay.done (see the package teardown invariant).
type carried struct {
	outbound chan frame
}

// frame is one dispatched frame, applied in arrival order.
type frame struct {
	data       []byte
	closeWrite bool
	closeConn  bool
}

// NewRelay builds the relay over conn (the donated end of the AF_UNIX pair).
// Call Serve to run the reader/mux.
func NewRelay(conn net.Conn, dial Dialer) *Relay {
	return &Relay{
		conn:   conn,
		dial:   dial,
		done:   make(chan struct{}),
		conns:  make(map[uint32]*carried),
		guests: make(map[uint32]net.Conn),
	}
}

func (r *Relay) failDone() {
	r.doneOnce.Do(func() { close(r.done) })
}

// write sends one raw frame; a write error fails the relay closed.
func (r *Relay) write(frameBytes []byte) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if _, err := r.conn.Write(frameBytes); err != nil {
		r.failDone()
		_ = r.conn.Close()
		go r.teardown()
		return err
	}
	return nil
}

// Serve runs the relay until the donated conn closes (the host tears it down
// at checkpoint/stop; restore donates a fresh FD), returning nil, or until a
// framing/protocol/write failure fails it closed, returning an error. It
// always closes the conn and every carried guest connection.
func (r *Relay) Serve() error {
	defer r.conn.Close()
	defer r.failDone()
	defer r.teardown()

	var lenBuf [4]byte
	for {
		if _, err := io.ReadFull(r.conn, lenBuf[:]); err != nil {
			if errors.Is(err, io.EOF) || r.isDone() {
				return nil // host closed the conn: clean teardown
			}
			return fmt.Errorf("ingress relay read: %w", err)
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if n < MinFrameBody || n > MaxFrameBody {
			return fmt.Errorf("ingress relay: invalid frame length %d", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r.conn, body); err != nil {
			if r.isDone() {
				return nil // torn down while reading: the conn is dead
			}
			return fmt.Errorf("ingress relay read body (%d): %w", n, err)
		}
		if body[0] != Version {
			return fmt.Errorf("ingress relay: protocol version %d, want %d", body[0], Version)
		}
		if err := r.dispatch(body); err != nil {
			return err
		}
	}
}

func (r *Relay) isDone() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// dispatch routes one decoded frame body.
func (r *Relay) dispatch(body []byte) error {
	kind := body[1]
	id := binary.BigEndian.Uint32(body[2:6])

	switch kind {
	case KindOpen:
		if len(body) != fixedHeaderLen {
			return fmt.Errorf("ingress relay: OPEN body length %d, want %d", len(body), fixedHeaderLen)
		}
		var ip16 [16]byte
		copy(ip16[:], body[6:22])
		port := binary.BigEndian.Uint16(body[22:24])
		cc := &carried{outbound: make(chan frame, queueDepth)}
		r.mu.Lock()
		if _, exists := r.conns[id]; exists {
			r.mu.Unlock()
			return fmt.Errorf("ingress relay: duplicate conn id %d", id)
		}
		// Registered before the dial starts, so host DATA racing the dial
		// queues instead of being dropped.
		r.conns[id] = cc
		r.mu.Unlock()
		go r.dialAndServe(cc, id, ip16, port)
		return nil
	case KindData:
		if len(body) < MinFrameBody {
			return fmt.Errorf("ingress relay: DATA body length %d, want >= %d", len(body), MinFrameBody)
		}
	case KindCloseWrite, KindClose:
		if len(body) != MinFrameBody {
			return fmt.Errorf("ingress relay: control body length %d, want %d", len(body), MinFrameBody)
		}
	default:
		return fmt.Errorf("ingress relay: unknown kind %d", kind)
	}

	r.mu.Lock()
	cc := r.conns[id]
	if kind == KindClose {
		// Retire the connection: later frames for the id drop silently.
		// The queue itself is never closed; the pump ends on the in-order
		// closeConn frame below.
		delete(r.conns, id)
	}
	r.mu.Unlock()
	if cc == nil {
		// Unknown id after a racing CLOSE: drop silently, never fail the
		// whole relay for per-connection races.
		return nil
	}
	f := frame{
		data:       body[6:],
		closeWrite: kind == KindCloseWrite,
		closeConn:  kind == KindClose,
	}
	for {
		select {
		case cc.outbound <- f:
			return nil
		case <-r.done:
			// Teardown fired while the queue was full (the mux was
			// blocked applying backpressure): exit instead of parking
			// forever.
			return ErrRelayClosed
		case <-time.After(peerCheckInterval):
			// Parked too long: the guest may be stalled while the host
			// has gone away. Probe the conn without consuming stream
			// data; if it is dead, fail the relay closed so teardown can
			// run. Otherwise keep applying backpressure.
			if r.peerGone() {
				r.failDone()
				_ = r.conn.Close()
				go r.teardown()
				return ErrRelayClosed
			}
		}
	}
}

// dialAndServe dials the guest target, reports the outcome, and pumps. The
// queue cc is registered at OPEN time by dispatch, so host DATA racing the
// dial queues instead of being dropped.
func (r *Relay) dialAndServe(cc *carried, id uint32, targetIP [16]byte, targetPort uint16) {
	guest, err := r.dial(targetIP, targetPort)
	if err != nil {
		_ = r.write(MarshalDialResult(id, DialRefused))
		_ = r.write(MarshalClose(id))
		r.forget(id)
		return
	}
	if werr := r.write(MarshalDialResult(id, DialOK)); werr != nil {
		_ = guest.Close()
		r.forget(id)
		return
	}

	r.mu.Lock()
	r.guests[id] = guest
	r.mu.Unlock()

	// Guest→host: read the guest conn, forward DATA, EOF half-closes.
	go func() {
		buf := make([]byte, MaxDataChunk)
		for {
			n, rerr := guest.Read(buf)
			if n > 0 {
				if werr := r.write(MarshalData(id, buf[:n])); werr != nil {
					_ = guest.Close()
					return
				}
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					_ = r.write(MarshalCloseWrite(id))
				} else {
					_ = r.write(MarshalClose(id))
				}
				_ = guest.Close()
				r.forget(id)
				return
			}
		}
	}()

	// Host→guest: drain the outbound queue into the guest conn, in order. A
	// full queue blocks the mux — the backpressure contract. The loop ends
	// on the in-order closeConn frame, or via done when the relay tears
	// down; either way the guest conn ends.
	tick := time.NewTicker(peerCheckInterval)
	defer tick.Stop()
	for {
		select {
		case f := <-cc.outbound:
			if len(f.data) > 0 {
				if _, werr := guest.Write(f.data); werr != nil {
					_ = guest.Close()
					r.forget(id)
					return
				}
			}
			if f.closeWrite {
				// Half-close toward the guest: no more host data will
				// arrive.
				if g, ok := guest.(interface{ CloseWrite() error }); ok {
					_ = g.CloseWrite()
				}
			}
			if f.closeConn {
				_ = guest.Close()
				r.forget(id)
				return
			}
		case <-r.done:
			_ = guest.Close()
			r.forget(id)
			return
		case <-tick.C:
			if r.peerGone() {
				// Host teardown on a stalled connection: fail closed. The
				// deferred teardown of Serve (or the write-failure path)
				// ends every remaining connection.
				r.failDone()
				_ = r.conn.Close()
				_ = guest.Close()
				r.forget(id)
				return
			}
		}
	}
}

// forget unregisters a connection and closes its guest conn. Idempotent.
func (r *Relay) forget(id uint32) {
	r.mu.Lock()
	delete(r.conns, id)
	guest := r.guests[id]
	delete(r.guests, id)
	r.mu.Unlock()
	if guest != nil {
		_ = guest.Close()
	}
}

// teardown ends every carried connection by closing the guest conns (waking
// blocked guest IO and the pumps writing to them); the pumps then exit via
// done. Queues are never closed (see the package teardown invariant).
func (r *Relay) teardown() {
	r.mu.Lock()
	guests := r.guests
	r.conns = make(map[uint32]*carried)
	r.guests = make(map[uint32]net.Conn)
	r.mu.Unlock()
	for _, g := range guests {
		_ = g.Close()
	}
}

// peerGone reports whether the donated conn is known-dead WITHOUT consuming
// stream data: a zero-timeout poll for POLLRDHUP, which fires on host close
// even while unread frames still mask a plain read's EOF. Best effort: an
// unusable conn object counts as gone, an inconclusive poll counts as alive
// (the read loop remains the authoritative death detector).
func (r *Relay) peerGone() bool {
	sc, ok := r.conn.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return true // the conn object is closed
	}
	gone := false
	if cerr := raw.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLRDHUP}}
		if _, errno := unix.Poll(fds, 0); errno != nil {
			return
		}
		if fds[0].Revents&(unix.POLLRDHUP|unix.POLLHUP|unix.POLLERR) != 0 {
			gone = true
		}
	}); cerr != nil {
		return true
	}
	return gone
}

// --- frame marshaling (frozen; golden-tested) -------------------------------

// MarshalOpen returns the OPEN frame announcing connID for the target IP
// (v4-in-v6 wire form) and port. The Sentry-side relay only decodes OPEN;
// this exists for tests and for golden-vector interop with the oca host
// relay.
func MarshalOpen(connID uint32, target [16]byte, port uint16) []byte {
	body := make([]byte, fixedHeaderLen)
	body[0] = Version
	body[1] = KindOpen
	binary.BigEndian.PutUint32(body[2:6], connID)
	copy(body[6:22], target[:])
	binary.BigEndian.PutUint16(body[22:24], port)
	return withLen(body)
}

// MarshalData returns the DATA frame carrying payload for connID.
func MarshalData(connID uint32, payload []byte) []byte {
	body := make([]byte, MinFrameBody+len(payload))
	body[0] = Version
	body[1] = KindData
	binary.BigEndian.PutUint32(body[2:6], connID)
	copy(body[6:], payload)
	return withLen(body)
}

// MarshalCloseWrite returns the half-close frame for connID.
func MarshalCloseWrite(connID uint32) []byte {
	return marshalControl(KindCloseWrite, connID)
}

// MarshalClose returns the full-teardown frame for connID.
func MarshalClose(connID uint32) []byte {
	return marshalControl(KindClose, connID)
}

// MarshalDialResult returns the dial-outcome frame for connID (DialOK or
// DialRefused).
func MarshalDialResult(connID uint32, status uint8) []byte {
	body := make([]byte, 7)
	body[0] = Version
	body[1] = KindDialResult
	binary.BigEndian.PutUint32(body[2:6], connID)
	body[6] = status
	return withLen(body)
}

func marshalControl(kind uint8, connID uint32) []byte {
	body := make([]byte, MinFrameBody)
	body[0] = Version
	body[1] = kind
	binary.BigEndian.PutUint32(body[2:6], connID)
	return withLen(body)
}

// withLen prefixes a body with its big-endian uint32 length.
func withLen(body []byte) []byte {
	frameBytes := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frameBytes, uint32(len(body)))
	copy(frameBytes[4:], body)
	return frameBytes
}
