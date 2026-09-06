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

// Package ingress carries host-accepted TCP connections into a sandbox over
// a donated Unix stream socket. The host owns the listening sockets and chooses
// each destination; the relay uses its Dialer to reach the sandbox network.
//
// Version 1 frames have a big-endian uint32 body length followed by a version
// byte, kind byte, and big-endian uint32 connection ID. OPEN adds a 16-byte IP
// address and uint16 port; DIAL_RESULT adds a status byte; DATA adds payload.
// CLOSE_WRITE ends only the sender's data direction. CLOSE cancels the whole
// connection, including any pending dial or queued data. A final CLOSE from the
// relay acknowledges teardown; only then may the host reuse that connection ID.
// This wire format is compatible with github.com/0xCarbon/oca/network/ingress.go.
//
// Queues and the number of carried connections are bounded. A full queue
// applies backpressure to the whole transport. Malformed frames fail the relay
// closed. Close cancels dials, closes all connections, and waits for workers;
// per-connection queues are never closed, so teardown cannot race a queue send.
package ingress

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"
)

const (
	Version        uint8 = 1
	KindOpen       uint8 = 1
	KindDialResult uint8 = 2
	KindData       uint8 = 3
	KindCloseWrite uint8 = 4
	KindClose      uint8 = 5
	DialOK         uint8 = 1
	DialRefused    uint8 = 2
	fixedHeaderLen       = 1 + 1 + 4 + 16 + 2
	MinFrameBody         = 6
	MaxDataChunk         = 64 * 1024
	MaxFrameBody         = fixedHeaderLen + MaxDataChunk
	// MaxConnections bounds live connections, including pending dials.
	// Excess OPENs receive DialRefused without disturbing existing connections.
	MaxConnections    = 1024
	queueDepth        = 16
	peerCheckInterval = 250 * time.Millisecond
)

// ErrRelayClosed reports that the transport is closed.
var ErrRelayClosed = errors.New("ingress relay closed")

// Dialer connects a requested target within the sandbox. It must honor context
// cancellation. Its connection must unblock IO on Close; implementing
// CloseWrite() error also allows TCP half-closes to be preserved.
type Dialer func(context.Context, [16]byte, uint16) (net.Conn, error)

// Relay multiplexes carried connections over a donated Unix stream socket.
// Call Serve once. Close is safe concurrently with Serve, including before it.
type Relay struct {
	conn         net.Conn
	dial         Dialer
	ctx          context.Context
	cancel       context.CancelFunc
	shutdownOnce sync.Once
	writeMu      sync.Mutex
	mu           sync.Mutex
	started      bool
	err          error
	conns        map[uint32]*carried
	workers      sync.WaitGroup
}

type carried struct {
	id       uint32
	ctx      context.Context
	cancel   context.CancelFunc
	outbound chan frame
	// closing is protected by Relay.mu; later frames are discarded after CLOSE.
	closing bool
	mu      sync.Mutex
	guest   net.Conn
}

type frame struct {
	data       []byte
	closeWrite bool
}

// NewRelay takes ownership of conn. The caller must either serve or close it.
func NewRelay(conn net.Conn, dial Dialer) *Relay {
	ctx, cancel := context.WithCancel(context.Background())
	return &Relay{conn: conn, dial: dial, ctx: ctx, cancel: cancel, conns: make(map[uint32]*carried)}
}

func (cc *carried) close() {
	cc.cancel()
	cc.mu.Lock()
	guest := cc.guest
	cc.mu.Unlock()
	if guest != nil {
		_ = guest.Close()
	}
}

func (cc *carried) setGuest(guest net.Conn) bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.ctx.Err() != nil {
		_ = guest.Close()
		return false
	}
	cc.guest = guest
	return true
}

// shutdown does not wait, so workers can invoke it on transport failure.
func (r *Relay) shutdown(err error) {
	r.shutdownOnce.Do(func() {
		r.mu.Lock()
		r.err = err
		r.cancel()
		conns := make([]*carried, 0, len(r.conns))
		for _, cc := range r.conns {
			conns = append(conns, cc)
		}
		r.mu.Unlock()
		_ = r.conn.Close()
		for _, cc := range conns {
			cc.close()
		}
	})
}

// Close stops the relay and waits until all dials and guest IO have ended.
func (r *Relay) Close() error {
	r.shutdown(nil)
	r.workers.Wait()
	return nil
}

func (r *Relay) writeLocked(data []byte) error {
	if r.ctx.Err() != nil {
		return ErrRelayClosed
	}
	n, err := r.conn.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		r.shutdown(fmt.Errorf("ingress relay write: %w", err))
	}
	return err
}

func (r *Relay) write(data []byte) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	return r.writeLocked(data)
}

// Serve returns nil on host closure or Close, and the first transport/protocol
// error on failure. No dial or guest IO worker survives its return.
func (r *Relay) Serve() error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return errors.New("ingress relay already served")
	}
	r.started = true
	if r.ctx.Err() != nil {
		err := r.err
		r.mu.Unlock()
		return err
	}
	r.workers.Add(1)
	r.mu.Unlock()
	go r.monitorPeer()
	err := r.serve()
	r.shutdown(err)
	r.workers.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *Relay) monitorPeer() {
	defer r.workers.Done()
	tick := time.NewTicker(peerCheckInterval)
	defer tick.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-tick.C:
			// Observe host teardown even when a full queue prevents reads.
			if peerGone(r.conn) {
				r.shutdown(nil)
				return
			}
		}
	}
}

func (r *Relay) serve() error {
	var lenBuf [4]byte
	for {
		if _, err := io.ReadFull(r.conn, lenBuf[:]); err != nil {
			if errors.Is(err, io.EOF) || r.ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ingress relay read: %w", err)
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if n < MinFrameBody || n > MaxFrameBody {
			return fmt.Errorf("ingress relay: invalid frame length %d", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r.conn, body); err != nil {
			if r.ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ingress relay read body: %w", err)
		}
		if body[0] != Version {
			return fmt.Errorf("ingress relay: protocol version %d, want %d", body[0], Version)
		}
		if err := r.dispatch(body); err != nil {
			return err
		}
	}
}

func (r *Relay) dispatch(body []byte) error {
	kind, id := body[1], binary.BigEndian.Uint32(body[2:6])
	switch kind {
	case KindOpen:
		if len(body) != fixedHeaderLen {
			return fmt.Errorf("ingress relay: OPEN body length %d", len(body))
		}
		var target [16]byte
		copy(target[:], body[6:22])
		port := binary.BigEndian.Uint16(body[22:24])
		r.mu.Lock()
		if r.ctx.Err() != nil {
			r.mu.Unlock()
			return nil
		}
		if _, exists := r.conns[id]; exists {
			r.mu.Unlock()
			return fmt.Errorf("ingress relay: duplicate conn id %d", id)
		}
		if len(r.conns) >= MaxConnections {
			r.mu.Unlock()
			if err := r.write(MarshalDialResult(id, DialRefused)); err != nil {
				return err
			}
			return r.write(MarshalClose(id))
		}
		ctx, cancel := context.WithCancel(r.ctx)
		cc := &carried{id: id, ctx: ctx, cancel: cancel, outbound: make(chan frame, queueDepth)}
		r.conns[id] = cc
		// Add under mu so Close cannot wait before this worker is registered.
		r.workers.Add(1)
		r.mu.Unlock()
		go r.dialAndServe(cc, target, port)
		return nil
	case KindData:
		if len(body)-MinFrameBody > MaxDataChunk {
			return fmt.Errorf("ingress relay: DATA payload too large: %d", len(body)-MinFrameBody)
		}
	case KindCloseWrite, KindClose:
		if len(body) != MinFrameBody {
			return fmt.Errorf("ingress relay: control body length %d", len(body))
		}
	default:
		return fmt.Errorf("ingress relay: unknown kind %d", kind)
	}
	r.mu.Lock()
	cc := r.conns[id]
	if cc == nil || cc.closing {
		r.mu.Unlock()
		return nil
	}
	if kind == KindClose {
		cc.closing = true
	}
	r.mu.Unlock()
	if kind == KindClose {
		cc.close()
		return nil
	}
	select {
	case cc.outbound <- frame{data: body[6:], closeWrite: kind == KindCloseWrite}:
		return nil
	case <-cc.ctx.Done():
		return nil
	}
}

// retire publishes the final CLOSE only after both guest pumps have stopped.
// Serialize removal with that frame so a reused ID cannot receive stale frames.
func (r *Relay) retire(cc *carried) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	if r.conns[cc.id] != cc {
		r.mu.Unlock()
		return
	}
	delete(r.conns, cc.id)
	r.mu.Unlock()
	_ = r.writeLocked(MarshalClose(cc.id))
}

func (r *Relay) dialAndServe(cc *carried, target [16]byte, port uint16) {
	defer r.workers.Done()
	defer r.retire(cc)
	defer cc.close()
	guest, err := r.dial(cc.ctx, target, port)
	if err != nil || guest == nil {
		if guest != nil {
			_ = guest.Close()
		}
		if cc.ctx.Err() == nil {
			_ = r.write(MarshalDialResult(cc.id, DialRefused))
		}
		return
	}
	if !cc.setGuest(guest) {
		return
	}
	if err := r.write(MarshalDialResult(cc.id, DialOK)); err != nil {
		return
	}

	// Closing the guest unblocks both directions. Join the reader before
	// retiring this ID, preventing stale DATA/CLOSE frames after ID reuse.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, MaxDataChunk)
		for {
			n, err := guest.Read(buf)
			if n > 0 {
				if werr := r.write(MarshalData(cc.id, buf[:n])); werr != nil {
					cc.close()
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) && cc.ctx.Err() == nil {
					if werr := r.write(MarshalCloseWrite(cc.id)); werr != nil {
						cc.close()
					}
				} else {
					cc.close()
				}
				return
			}
		}
	}()
	defer func() { cc.close(); <-readDone }()

	hostEOF, guestEOF := false, false
	readerDone := (<-chan struct{})(readDone)
	for {
		select {
		case <-cc.ctx.Done():
			return
		case <-readerDone:
			guestEOF = true
			readerDone = nil
			if hostEOF {
				return
			}
		case f := <-cc.outbound:
			if len(f.data) > 0 {
				if hostEOF {
					return
				}
				n, err := guest.Write(f.data)
				if err != nil || n != len(f.data) {
					return
				}
			}
			if f.closeWrite && !hostEOF {
				halfCloser, ok := guest.(interface{ CloseWrite() error })
				if !ok || halfCloser.CloseWrite() != nil {
					return
				}
				hostEOF = true
				if guestEOF {
					return
				}
			}
		}
	}
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
	if uint64(len(body)) > math.MaxUint32 {
		panic("ingress frame exceeds uint32 length")
	}
	frameBytes := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frameBytes, uint32(len(body)))
	copy(frameBytes[4:], body)
	return frameBytes
}
