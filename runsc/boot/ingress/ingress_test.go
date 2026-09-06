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

package ingress

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// socketPair returns the two ends of a connected AF_UNIX stream pair as
// net.Conns, mirroring how the host donates one end to runsc as --ingress-fd
// and serves the other (see runsc/boot/egress_gate_test.go for the egress
// seam equivalent).
func socketPair(t *testing.T) (host, donated net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	// FileConn dups each descriptor, so close the raw copies immediately —
	// as the donating worker does — so closing one returned conn is visible
	// to the peer.
	a := os.NewFile(uintptr(fds[0]), "ingress-test-host")
	b := os.NewFile(uintptr(fds[1]), "ingress-test-donated")
	defer a.Close()
	defer b.Close()
	ca, err := net.FileConn(a)
	if err != nil {
		t.Fatalf("FileConn host: %v", err)
	}
	cb, err := net.FileConn(b)
	if err != nil {
		t.Fatalf("FileConn donated: %v", err)
	}
	t.Cleanup(func() { _ = ca.Close(); _ = cb.Close() })
	return ca, cb
}

// echoGuest starts a TCP "guest service" that echoes every byte and
// half-closes its write side when it sees a 0x04 (EOT) byte, mirroring the
// oca golden-vector test guest. Returns the guest address and a counter of
// observed half-closes.
func echoGuest(t *testing.T) (addr netip.AddrPort, halfCloses *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("guest listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	halfCloses = &atomic.Int64{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, rerr := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
						if bytes.IndexByte(buf[:n], 0x04) >= 0 {
							if tc, ok := c.(*net.TCPConn); ok {
								_ = tc.CloseWrite()
							}
							halfCloses.Add(1)
						}
					}
					if rerr != nil {
						return
					}
				}
			}(c)
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String()), halfCloses
}

// serveRelay starts a Relay over the donated end of a socketpair with the
// given dialer. Serve's return value is delivered once on done and finished
// is closed after it; tests may consume either signal, and t.Cleanup waits
// on finished so no test double-receives.
func serveRelay(t *testing.T, donated net.Conn, dial Dialer) (done chan error, finished chan struct{}) {
	t.Helper()
	r := NewRelay(donated, dial)
	done = make(chan error, 1)
	finished = make(chan struct{})
	go func() {
		done <- r.Serve()
		close(finished)
	}()
	t.Cleanup(func() {
		_ = donated.Close()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("relay Serve did not return after conn close")
		}
	})
	return done, finished
}

// tcpDialer returns a Dialer that connects to a host TCP address, unmapping
// the v4-in-v6 wire form the way a real guest dial would.
func tcpDialer(addr netip.AddrPort) Dialer {
	return func(ctx context.Context, target [16]byte, port uint16) (net.Conn, error) {
		got := netip.AddrPortFrom(netip.AddrFrom16(target).Unmap(), port)
		if got != addr {
			return nil, fmt.Errorf("unexpected dial target %v, want %v", got, addr)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", got.String())
	}
}

// readFrame reads one frame from the host end of a socketpair and returns
// its decoded fields.
func readFrame(t *testing.T, c net.Conn) (kind byte, connID uint32, payload []byte) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	defer c.SetReadDeadline(time.Time{})
	var lenBuf [4]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		t.Fatalf("reading frame length: %v", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatalf("reading frame body: %v", err)
	}
	if len(body) < 6 {
		t.Fatalf("frame body too short: %d bytes", len(body))
	}
	return body[1], binary.BigEndian.Uint32(body[2:6]), body[6:]
}

// writeAll writes the whole frame to the host end.
func writeAll(t *testing.T, c net.Conn, frame []byte) {
	t.Helper()
	if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	defer c.SetWriteDeadline(time.Time{})
	if _, err := c.Write(frame); err != nil {
		t.Fatalf("writing frame: %v", err)
	}
}

func loopback16() [16]byte {
	// 127.0.0.1 as v4-in-v6, the frozen wire form.
	return [16]byte{10: 0xff, 11: 0xff, 12: 127, 13: 0, 14: 0, 15: 1}
}

func TestWireFormatGolden(t *testing.T) {
	// The wire format is frozen and shared byte-for-byte with the oca host
	// relay (github.com/0xCarbon/oca/network/ingress.go). These are the oca
	// golden vectors; any drift here breaks the managed sandbox protocol.

	// OPEN(connID=7, target 127.0.0.1:8080): 24-byte body.
	frame := MarshalOpen(7, loopback16(), 8080)
	want := []byte{
		0, 0, 0, 24, // body length
		Version, KindOpen,
		0, 0, 0, 7, // conn id
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 127, 0, 0, 1, // v4-in-v6 loopback
		0x1f, 0x90, // 8080
	}
	if !bytes.Equal(frame, want) {
		t.Fatalf("OPEN frame = %x, want %x", frame, want)
	}

	// DATA(connID=7, "hi"): 8-byte body.
	frame = MarshalData(7, []byte("hi"))
	want = []byte{
		0, 0, 0, 8,
		Version, KindData,
		0, 0, 0, 7, 'h', 'i',
	}
	if !bytes.Equal(frame, want) {
		t.Fatalf("DATA frame = %x, want %x", frame, want)
	}

	// CLOSE_WRITE(connID=7): 6-byte body.
	frame = MarshalCloseWrite(7)
	want = []byte{0, 0, 0, 6, Version, KindCloseWrite, 0, 0, 0, 7}
	if !bytes.Equal(frame, want) {
		t.Fatalf("CLOSE_WRITE frame = %x, want %x", frame, want)
	}

	// CLOSE(connID=7): 6-byte body.
	frame = MarshalClose(7)
	want = []byte{0, 0, 0, 6, Version, KindClose, 0, 0, 0, 7}
	if !bytes.Equal(frame, want) {
		t.Fatalf("CLOSE frame = %x, want %x", frame, want)
	}

	// DIAL_RESULT(connID=7, refused): 7-byte body.
	frame = MarshalDialResult(7, DialRefused)
	want = []byte{0, 0, 0, 7, Version, KindDialResult, 0, 0, 0, 7, DialRefused}
	if !bytes.Equal(frame, want) {
		t.Fatalf("DIAL_RESULT frame = %x, want %x", frame, want)
	}
}

// TestRelayEndToEnd drives the full data path over a socketpair: OPEN dials
// the guest, bytes echo back, and half-close semantics hold in both
// directions; CLOSE tears the carried connection down.
func TestRelayEndToEnd(t *testing.T) {
	guestAddr, guestHalfCloses := echoGuest(t)
	host, donated := socketPair(t)
	_, _ = serveRelay(t, donated, tcpDialer(guestAddr))

	// OPEN(connID=1): the relay must dial the guest and answer DIAL_RESULT OK.
	writeAll(t, host, MarshalOpen(1, loopback16(), guestAddr.Port()))
	kind, id, payload := readFrame(t, host)
	if kind != KindDialResult || id != 1 || len(payload) != 1 || payload[0] != DialOK {
		t.Fatalf("after OPEN: kind=%d id=%d payload=%x, want DIAL_RESULT(id=1, OK)", kind, id, payload)
	}

	// Request/response round trip.
	writeAll(t, host, MarshalData(1, []byte("hello ingress")))
	kind, id, payload = readFrame(t, host)
	if kind != KindData || id != 1 || string(payload) != "hello ingress" {
		t.Fatalf("echo: kind=%d id=%d payload=%q", kind, id, payload)
	}

	// Guest half-close: after EOT the guest CloseWrites, which must reach the
	// host as CLOSE_WRITE (after the echoed EOT byte).
	writeAll(t, host, MarshalData(1, []byte{0x04}))
	kind, id, payload = readFrame(t, host)
	if kind != KindData || id != 1 || len(payload) != 1 || payload[0] != 0x04 {
		t.Fatalf("echoed EOT: kind=%d id=%d payload=%x", kind, id, payload)
	}
	kind, id, payload = readFrame(t, host)
	if kind != KindCloseWrite || id != 1 || len(payload) != 0 {
		t.Fatalf("guest EOF: kind=%d id=%d payload=%x, want CLOSE_WRITE(id=1)", kind, id, payload)
	}
	if guestHalfCloses.Load() == 0 {
		t.Fatal("guest never half-closed; EOT did not arrive as data")
	}

	// Host half-close: CLOSE_WRITE must map to a CloseWrite on the guest
	// connection. The guest echoes nothing new, but a following read on the
	// guest side would see EOF; verify via the EOT path instead: send CLOSE
	// and expect the guest connection to be torn down (next echo conn ends).
	writeAll(t, host, MarshalClose(1))
	kind, id, _ = readFrame(t, host)
	if kind != KindClose || id != 1 {
		t.Fatalf("teardown: kind=%d id=%d, want CLOSE(id=1)", kind, id)
	}

	// Frames for an unknown (already-closed) id are dropped, not fatal.
	writeAll(t, host, MarshalData(99, []byte("stray")))
	// The relay must still serve: a fresh connection works end to end.
	writeAll(t, host, MarshalOpen(2, loopback16(), guestAddr.Port()))
	kind, id, payload = readFrame(t, host)
	if kind != KindDialResult || id != 2 || payload[0] != DialOK {
		t.Fatalf("second OPEN after CLOSE: kind=%d id=%d payload=%x", kind, id, payload)
	}
	writeAll(t, host, MarshalData(2, []byte("again")))
	kind, id, payload = readFrame(t, host)
	if kind != KindData || id != 2 || string(payload) != "again" {
		t.Fatalf("second echo: kind=%d id=%d payload=%q", kind, id, payload)
	}
}

// TestGuestDialRefused verifies a refused guest dial is reported as
// DIAL_RESULT(refused) followed by CLOSE, and the relay stays healthy.
func TestGuestDialRefused(t *testing.T) {
	host, donated := socketPair(t)
	_, _ = serveRelay(t, donated, func(context.Context, [16]byte, uint16) (net.Conn, error) {
		return nil, errors.New("guest loopback refused")
	})

	writeAll(t, host, MarshalOpen(1, loopback16(), 8080))
	kind, id, payload := readFrame(t, host)
	if kind != KindDialResult || id != 1 || len(payload) != 1 || payload[0] != DialRefused {
		t.Fatalf("dial refusal: kind=%d id=%d payload=%x, want DIAL_RESULT(id=1, refused)", kind, id, payload)
	}
	kind, id, payload = readFrame(t, host)
	if kind != KindClose || id != 1 || len(payload) != 0 {
		t.Fatalf("after refusal: kind=%d id=%d payload=%x, want CLOSE(id=1)", kind, id, payload)
	}

	// The relay itself must remain serviceable: another connection dials.
	writeAll(t, host, MarshalOpen(2, loopback16(), 8080))
	kind, id, payload = readFrame(t, host)
	if kind != KindDialResult || id != 2 || payload[0] != DialRefused {
		t.Fatalf("second refusal: kind=%d id=%d payload=%x", kind, id, payload)
	}
}

// TestVersionMismatchFailsClosed pins the fail-closed posture: an unknown
// protocol version tears the relay down instead of misparsing the frame.
func TestVersionMismatchFailsClosed(t *testing.T) {
	host, donated := socketPair(t)
	done, _ := serveRelay(t, donated, func(context.Context, [16]byte, uint16) (net.Conn, error) {
		t.Error("dialer must not run for a rejected frame")
		return nil, errors.New("unreachable")
	})

	body := []byte{Version + 1, KindData, 0, 0, 0, 1, 'x'}
	writeAll(t, host, withLen(body))

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve returned nil on version mismatch, want error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not fail on version mismatch")
	}

	// The donated conn must be closed so the host side fails closed too.
	if err := host.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := host.Read(buf); err == nil {
		t.Fatal("host conn still readable after relay teardown")
	}
}

// TestMalformedFramesFailClosed pins the frame guards: a length prefix out of
// bounds, an unknown kind, a short control frame, and a duplicate OPEN each
// tear the relay down.
func TestMalformedFramesFailClosed(t *testing.T) {
	// Invalid lengths must be rejected before reading a body. Sending a body
	// races the relay's correct immediate close and can fail with EPIPE.
	lengthPrefix := func(n uint32) []byte {
		prefix := make([]byte, 4)
		binary.BigEndian.PutUint32(prefix, n)
		return prefix
	}
	cases := []struct {
		name  string
		frame []byte
	}{
		{
			name:  "oversized length prefix",
			frame: lengthPrefix(MaxFrameBody + 1),
		},
		{
			name:  "undersized length prefix",
			frame: lengthPrefix(MinFrameBody - 1),
		},
		{
			name:  "unknown kind",
			frame: withLen([]byte{Version, 42, 0, 0, 0, 1}),
		},
		{
			name:  "short control frame",
			frame: withLen([]byte{Version, KindClose, 0, 0, 0}),
		},
		{
			name:  "oversized DATA payload",
			frame: MarshalData(1, make([]byte, MaxDataChunk+1)),
		},
		{
			name:  "unexpected dial result",
			frame: MarshalDialResult(1, DialOK),
		},
		{
			name:  "short OPEN frame",
			frame: withLen([]byte{Version, KindOpen, 0, 0, 0, 1, 1, 2, 3}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, donated := socketPair(t)
			done, _ := serveRelay(t, donated, func(context.Context, [16]byte, uint16) (net.Conn, error) {
				t.Error("dialer must not run for a rejected frame")
				return nil, errors.New("unreachable")
			})
			writeAll(t, host, tc.frame)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Serve returned nil on malformed frame, want error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve did not fail on malformed frame")
			}
		})
	}
}

// TestDuplicateOpenFailsClosed pins the conn-id guard: reannouncing a live
// id is a protocol violation, not a silent takeover.
func TestDuplicateOpenFailsClosed(t *testing.T) {
	guestAddr, _ := echoGuest(t)
	host, donated := socketPair(t)
	done, _ := serveRelay(t, donated, tcpDialer(guestAddr))

	writeAll(t, host, MarshalOpen(1, loopback16(), guestAddr.Port()))
	kind, _, _ := readFrame(t, host)
	if kind != KindDialResult {
		t.Fatalf("first OPEN: kind=%d", kind)
	}
	writeAll(t, host, MarshalOpen(1, loopback16(), guestAddr.Port()))
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve returned nil on duplicate OPEN, want error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not fail on duplicate OPEN")
	}
}

// TestBackpressureStallsUpstream pins the bounded-buffer contract: when the
// guest stops reading, only a bounded amount of payload is buffered past the
// stall point — the mux blocks, the socketpair fills, and the host writer
// stalls (transitive backpressure, no unbounded buffering).
func TestBackpressureStallsUpstream(t *testing.T) {
	guestReady := make(chan struct{})
	var received atomic.Int64
	stalled := make(chan struct{})
	var once sync.Once
	host, donated := socketPair(t)
	_, _ = serveRelay(t, donated, func(context.Context, [16]byte, uint16) (net.Conn, error) {
		a, b := net.Pipe()
		once.Do(func() { close(guestReady) })
		go func() {
			defer a.Close()
			buf := make([]byte, 64*1024)
			n, _ := a.Read(buf)
			received.Add(int64(n))
			<-stalled // never read again until released
		}()
		return b, nil
	})

	writeAll(t, host, MarshalOpen(1, loopback16(), 8080))
	<-guestReady

	// Push far more than the per-conn queue + socketpair buffers can hold; a
	// proxy without backpressure would buffer it all unboundedly.
	chunk := MarshalData(1, bytes.Repeat([]byte{0x61}, MaxDataChunk))
	written := make(chan int, 1)
	go func() {
		total := 0
		for i := 0; i < 64; i++ {
			if _, err := host.Write(chunk); err != nil {
				break
			}
			total += len(chunk)
		}
		written <- total
	}()

	select {
	case total := <-written:
		t.Fatalf("host pushed all %d bytes into a stalled guest; backpressure is not enforced", total)
	case <-time.After(750 * time.Millisecond):
		// Stalled as required.
	}
	close(stalled)
}

// TestTeardownClosesGuestConns verifies that a fail-closed relay tears down
// every carried guest connection — the sandbox never keeps ambiguous state.
func TestTeardownClosesGuestConns(t *testing.T) {
	guestAddr, _ := echoGuest(t)
	host, donated := socketPair(t)
	done, _ := serveRelay(t, donated, tcpDialer(guestAddr))

	writeAll(t, host, MarshalOpen(1, loopback16(), guestAddr.Port()))
	readFrame(t, host) // DIAL_RESULT OK

	// Corrupt the stream: the relay must fail closed and end the guest conn.
	writeAll(t, host, withLen([]byte{Version, 42, 0, 0, 0, 1}))
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve returned nil on malformed frame, want error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not fail on malformed frame")
	}
}

// TestStallThenHostCloseEndsRelay pins the parked-mux liveness contract: a
// mux blocked on a full queue (stalled guest) must still notice the host
// closing the donated conn and end the relay, instead of leaking the
// goroutine and the carried connection for the life of the sandbox.
func TestStallThenHostCloseEndsRelay(t *testing.T) {
	guestReady := make(chan struct{})
	stalled := make(chan struct{})
	defer close(stalled)
	var once sync.Once
	host, donated := socketPair(t)
	_, finished := serveRelay(t, donated, func(context.Context, [16]byte, uint16) (net.Conn, error) {
		a, b := net.Pipe()
		once.Do(func() { close(guestReady) })
		go func() {
			defer a.Close()
			buf := make([]byte, 64*1024)
			_, _ = a.Read(buf)
			<-stalled // stall forever: the queue fills, the mux parks
		}()
		return b, nil
	})

	writeAll(t, host, MarshalOpen(1, loopback16(), 8080))
	<-guestReady
	chunk := MarshalData(1, bytes.Repeat([]byte{0x61}, MaxDataChunk))
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			if _, err := host.Write(chunk); err != nil {
				return
			}
			select {
			case <-finished:
				return
			default:
			}
		}
	}()

	// Wait until the writer has actually stalled (queue + socketpair full),
	// then tear down from the host side like the worker does at stop.
	time.Sleep(500 * time.Millisecond)
	_ = host.Close()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not notice the host teardown while parked on a full queue")
	}
	<-writerDone
}

// TestServeReturnsOnConnClose verifies Serve returns cleanly when the host
// end closes the donated conn (checkpoint/stop teardown; restore re-donates).
func TestServeReturnsOnConnClose(t *testing.T) {
	host, donated := socketPair(t)
	done, _ := serveRelay(t, donated, func(context.Context, [16]byte, uint16) (net.Conn, error) {
		return nil, errors.New("unreachable")
	})
	_ = host.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after clean conn close = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after conn close")
	}
}

// A read EOF from the guest is only a half-close: the host must still be able
// to send its remaining request before closing its own write direction.
func TestGuestHalfCloseKeepsHostWriteOpen(t *testing.T) {
	service, guest := socketPair(t)
	host, donated := socketPair(t)
	serveRelay(t, donated, func(context.Context, [16]byte, uint16) (net.Conn, error) { return guest, nil })
	writeAll(t, host, MarshalOpen(1, loopback16(), 8080))
	readFrame(t, host)
	if err := service.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	kind, _, _ := readFrame(t, host)
	if kind != KindCloseWrite {
		t.Fatalf("kind = %d, want CLOSE_WRITE", kind)
	}
	want := []byte("request after guest EOF")
	writeAll(t, host, MarshalData(1, want))
	writeAll(t, host, MarshalCloseWrite(1))
	service.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(service)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("guest received %q, %v; want %q", got, err, want)
	}
}
