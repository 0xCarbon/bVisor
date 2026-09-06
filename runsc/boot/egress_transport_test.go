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

package boot

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// faultGateConn provides replies even after transport errors. A fail-closed
// client must close it and never apply those bytes to subsequent requests.
type faultGateConn struct {
	net.Conn
	replies     *bytes.Reader
	deadlineErr error
	shortWrite  bool
	closed      bool
	writes      int
}

func TestEgressGateQueueDeadline(t *testing.T) {
	conn := &faultGateConn{replies: bytes.NewReader([]byte{egressGateVerdictAllow})}
	c := egressGateClientForConn(conn)
	t.Cleanup(c.Close)
	dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{1, 2, 3, 4}), Port: 443}
	c.turn <- struct{}{} // An earlier flow owns the connection.
	done := make(chan tcpip.Error, 1)
	go func() {
		_, err := c.checkUntil(egressGateKindTCP, dst, nil, time.Now().Add(20*time.Millisecond))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("queued flow was allowed after its deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queue wait did not respect the deadline")
	}
	<-c.turn
	if conn.writes != 0 || conn.closed {
		t.Fatalf("queue expiry touched the connection: writes=%d closed=%v", conn.writes, conn.closed)
	}
	if err := c.CheckTCP(dst); err != nil {
		t.Fatalf("queue expiry killed a healthy connection: %v", err)
	}
}

func TestEgressGateCloseUnblocksChecks(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	c := egressGateClientForConn(conn)
	defer c.Close()
	dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{1, 2, 3, 4}), Port: 443}
	results := make(chan tcpip.Error, 2)
	go func() { results <- c.CheckTCP(dst) }()
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(peer, make([]byte, 24)); err != nil {
		t.Fatalf("reading request: %v", err)
	}
	go func() { results <- c.CheckUDP(dst) }()
	// One check is waiting for a verdict; another may be waiting its turn.
	c.Close()
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err == nil {
				t.Error("closed gate allowed a flow")
			}
		case <-time.After(time.Second):
			t.Fatal("Close did not unblock the gate check")
		}
	}
}

func TestEgressGateReadTimeout(t *testing.T) {
	fg, c := newTestClient(t)
	dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{1, 2, 3, 4}), Port: 443}
	result := make(chan tcpip.Error, 1)
	go func() {
		_, err := c.checkUntil(egressGateKindTCP, dst, nil, time.Now().Add(100*time.Millisecond))
		result <- err
	}()
	fg.waitRequest()
	if err := <-result; err == nil {
		t.Fatal("unanswered request was allowed")
	}
	fg.verdicts <- egressGateVerdictAllow // Too late to apply to any flow.
	if err := c.CheckTCP(dst); err == nil {
		t.Fatal("late verdict allowed another flow")
	}
}

func TestEgressGateRejectsInvalidAddress(t *testing.T) {
	conn := &faultGateConn{replies: bytes.NewReader(nil)}
	c := egressGateClientForConn(conn)
	defer c.Close()
	if err := c.CheckTCP(tcpip.FullAddress{Port: 443}); err == nil {
		t.Fatal("invalid destination was allowed")
	}
	if conn.writes != 0 {
		t.Fatal("sent invalid destination to host")
	}
}

func TestEgressGateDenyKeepsConnection(t *testing.T) {
	conn := &faultGateConn{replies: bytes.NewReader([]byte{egressGateVerdictDeny, egressGateVerdictAllow})}
	c := egressGateClientForConn(conn)
	defer c.Close()
	dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{1, 2, 3, 4}), Port: 443}
	if err := c.CheckTCP(dst); err == nil {
		t.Error("host denial allowed a flow")
	}
	if err := c.CheckUDP(dst); err != nil {
		t.Errorf("valid denial killed the connection: %v", err)
	}
}

func TestEgressGateRejectsPacketSocket(t *testing.T) {
	for _, typ := range []int{unix.SOCK_DGRAM, unix.SOCK_SEQPACKET} {
		fds, err := unix.Socketpair(unix.AF_UNIX, typ|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fds[1])
		f := os.NewFile(uintptr(fds[0]), "invalid-gate")
		c, err := newEgressGateClientFile(f)
		if err == nil {
			c.Close()
			t.Errorf("accepted packet socket type %d as a stream", typ)
		}
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Errorf("donated file still open after rejection: %v", err)
		}
	}
}

func (c *faultGateConn) SetDeadline(time.Time) error { return c.deadlineErr }
func (c *faultGateConn) Read(p []byte) (int, error)  { return c.replies.Read(p) }
func (c *faultGateConn) Write(p []byte) (int, error) {
	c.writes++
	if c.shortWrite {
		return len(p) - 1, nil
	}
	return len(p), nil
}
func (c *faultGateConn) Close() error {
	c.closed = true
	return nil
}

func TestEgressGateTransportFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deadlineErr error
		shortWrite  bool
	}{
		{name: "deadline", deadlineErr: errors.New("deadline unsupported")},
		{name: "short write", shortWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &faultGateConn{
				replies:     bytes.NewReader([]byte{egressGateVerdictAllow, egressGateVerdictAllow}),
				deadlineErr: tc.deadlineErr,
				shortWrite:  tc.shortWrite,
			}
			c := egressGateClientForConn(conn)
			dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{1, 2, 3, 4}), Port: 443}
			if err := c.CheckTCP(dst); err == nil {
				t.Error("allowed a flow after transport failure")
			}
			if !conn.closed {
				t.Error("transport failure did not close the connection")
			}
			writes := conn.writes
			if err := c.CheckUDP(dst); err == nil {
				t.Error("applied a stale reply to a later flow")
			}
			if conn.writes != writes {
				t.Error("sent another request after transport failure")
			}
		})
	}
}

func TestEgressGateInvalidVerdictPoisonsConnection(t *testing.T) {
	for _, verdict := range []byte{egressGateVerdictNeedMore, 3, 0xff} {
		conn := &faultGateConn{replies: bytes.NewReader([]byte{verdict, egressGateVerdictAllow})}
		c := egressGateClientForConn(conn)
		dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{1, 2, 3, 4}), Port: 443}
		if err := c.CheckTCP(dst); err == nil {
			t.Errorf("verdict %d: invalid verdict allowed TCP", verdict)
		}
		if err := c.CheckUDP(dst); err == nil {
			t.Errorf("verdict %d: reused connection after invalid verdict", verdict)
		}
		if !conn.closed || conn.writes != 1 {
			t.Errorf("verdict %d: closed=%v writes=%d, want closed=true writes=1", verdict, conn.closed, conn.writes)
		}
	}
}
