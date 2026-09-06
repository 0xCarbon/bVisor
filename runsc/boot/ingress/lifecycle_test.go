// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestHostHalfCloseKeepsGuestResponseOpen(t *testing.T) {
	host, donated := socketPair(t)
	service, guest := socketPair(t)
	serveRelay(t, donated, func(context.Context, [16]byte, uint16) (net.Conn, error) { return guest, nil })
	writeAll(t, host, MarshalOpen(1, loopback16(), 8080))
	readFrame(t, host)
	writeAll(t, host, MarshalCloseWrite(1))
	service.SetDeadline(time.Now().Add(5 * time.Second))
	var b [1]byte
	if _, err := service.Read(b[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("guest did not receive EOF: %v", err)
	}
	writeAll(t, service, []byte("response after request EOF"))
	service.(*net.UnixConn).CloseWrite()
	kind, _, payload := readFrame(t, host)
	if kind != KindData || string(payload) != "response after request EOF" {
		t.Fatalf("response lost: kind=%d payload=%q", kind, payload)
	}
	kind, _, _ = readFrame(t, host)
	if kind != KindCloseWrite {
		t.Fatalf("got kind=%d, want CLOSE_WRITE", kind)
	}
	kind, _, _ = readFrame(t, host)
	if kind != KindClose {
		t.Fatalf("got kind=%d, want final CLOSE", kind)
	}
}

func TestCloseCancelsDialAndAllowsIDReuse(t *testing.T) {
	host, donated := socketPair(t)
	started := make(chan struct{}, 1)
	ended := make(chan struct{}, 1)
	serveRelay(t, donated, func(ctx context.Context, _ [16]byte, _ uint16) (net.Conn, error) {
		started <- struct{}{}
		<-ctx.Done()
		ended <- struct{}{}
		return nil, ctx.Err()
	})
	for i := 0; i < 100; i++ {
		writeAll(t, host, MarshalOpen(1, loopback16(), 8080))
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("dial did not start")
		}
		writeAll(t, host, MarshalClose(1))
		kind, id, _ := readFrame(t, host)
		if kind != KindClose || id != 1 {
			t.Fatalf("cancelled dial: kind=%d id=%d", kind, id)
		}
		select {
		case <-ended:
		default:
			t.Fatal("CLOSE published before dial ended")
		}
	}
}

func TestRelayCloseJoinsPendingDial(t *testing.T) {
	host, donated := socketPair(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	finishDial := make(chan struct{})
	service, guest := socketPair(t)
	r := NewRelay(donated, func(ctx context.Context, _ [16]byte, _ uint16) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-finishDial
		return guest, nil // A connect racing with cancellation still needs closing.
	})
	served := make(chan error, 1)
	go func() { served <- r.Serve() }()
	writeAll(t, host, MarshalOpen(1, loopback16(), 8080))
	<-started
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	<-cancelled
	select {
	case <-closed:
		t.Fatal("Close did not join pending dial")
	default:
	}
	close(finishDial)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	service.SetReadDeadline(time.Now().Add(5 * time.Second))
	var b [1]byte
	if _, err := service.Read(b[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("late dial leaked guest: %v", err)
	}
}

func TestConnectionLimitRefusesOnlyExcessOpen(t *testing.T) {
	host, donated := socketPair(t)
	started := make(chan struct{}, MaxConnections)
	serveRelay(t, donated, func(ctx context.Context, _ [16]byte, _ uint16) (net.Conn, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	for id := uint32(0); id < MaxConnections; id++ {
		writeAll(t, host, MarshalOpen(id, loopback16(), 8080))
	}
	for i := 0; i < MaxConnections; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("accepted dial missing")
		}
	}
	writeAll(t, host, MarshalOpen(MaxConnections, loopback16(), 8080))
	kind, id, payload := readFrame(t, host)
	if kind != KindDialResult || id != MaxConnections || !bytes.Equal(payload, []byte{DialRefused}) {
		t.Fatalf("excess open: kind=%d id=%d payload=%x", kind, id, payload)
	}
	kind, id, _ = readFrame(t, host)
	if kind != KindClose || id != MaxConnections {
		t.Fatalf("excess close: kind=%d id=%d", kind, id)
	}
	writeAll(t, host, MarshalClose(0))
	kind, id, _ = readFrame(t, host)
	if kind != KindClose || id != 0 {
		t.Fatalf("existing connection affected by limit: kind=%d id=%d", kind, id)
	}
	writeAll(t, host, MarshalOpen(MaxConnections, loopback16(), 8080))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("freed capacity was not reusable")
	}
}

func TestGuestAbortUnblocksFullQueue(t *testing.T) {
	host, donated := socketPair(t)
	service, guest := net.Pipe()
	defer service.Close()
	r := NewRelay(donated, func(context.Context, [16]byte, uint16) (net.Conn, error) { return guest, nil })
	defer r.Close()
	served := make(chan error, 1)
	go func() { served <- r.Serve() }()
	writeAll(t, host, MarshalOpen(1, loopback16(), 8080))
	readFrame(t, host)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; i < 64; i++ {
			if _, err := host.Write(MarshalData(1, make([]byte, MaxDataChunk))); err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		cc := r.conns[1]
		full := cc != nil && len(cc.outbound) == queueDepth
		r.mu.Unlock()
		if full {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queue never filled")
		}
		time.Sleep(time.Millisecond)
	}
	service.Close()
	select {
	case <-writerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("orphan queue kept mux blocked")
	}
	kind, _, _ := readFrame(t, host)
	if kind != KindCloseWrite && kind != KindClose {
		t.Fatalf("unexpected abort frame %d", kind)
	}
	if kind == KindCloseWrite {
		kind, _, _ = readFrame(t, host)
	}
	if kind != KindClose {
		t.Fatalf("missing final CLOSE: %d", kind)
	}
	host.Close()
	<-served
}

type failingWriteConn struct {
	net.Conn
	err error
}

func (c failingWriteConn) Write([]byte) (int, error) { return 0, c.err }
func TestTransportWriteFailureIsReported(t *testing.T) {
	sentinel := errors.New("transport write failed")
	for _, failure := range []error{sentinel, nil} {
		host, donated := socketPair(t)
		done, _ := serveRelay(t, failingWriteConn{donated, failure}, func(context.Context, [16]byte, uint16) (net.Conn, error) { return nil, errors.New("refused") })
		writeAll(t, host, MarshalOpen(1, loopback16(), 8080))
		want := failure
		if want == nil {
			want = io.ErrShortWrite
		}
		select {
		case err := <-done:
			if !errors.Is(err, want) {
				t.Fatalf("Serve = %v, want %v", err, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not stop after write failure")
		}
	}
}

// The in-memory transport makes fuzzing finite: reads end at EOF, writes cannot
// block, and all asynchronous dials are joined by Serve before the next input.
type frameStream struct{ *bytes.Reader }

func (c frameStream) Write(p []byte) (int, error)    { return len(p), nil }
func (frameStream) Close() error                     { return nil }
func (frameStream) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "fuzz", Net: "unix"} }
func (c frameStream) RemoteAddr() net.Addr           { return c.LocalAddr() }
func (frameStream) SetDeadline(time.Time) error      { return nil }
func (frameStream) SetReadDeadline(time.Time) error  { return nil }
func (frameStream) SetWriteDeadline(time.Time) error { return nil }
func FuzzFrames(f *testing.F) {
	for _, seed := range [][]byte{MarshalOpen(1, loopback16(), 8080), MarshalData(1, []byte("abc")), MarshalCloseWrite(1), MarshalClose(1), {0, 0, 0, 5}, {0, 0, 0, 8, Version, KindData}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 4*MaxFrameBody {
			t.Skip()
		}
		r := NewRelay(frameStream{bytes.NewReader(input)}, func(context.Context, [16]byte, uint16) (net.Conn, error) { return nil, errors.New("refused") })
		r.Serve()
		r.Close()
	})
}
