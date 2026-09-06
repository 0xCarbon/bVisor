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

package boot

import (
	stdcontext "context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/runsc/config"
)

// fakeGate serves one egress-gate connection from a unix socketpair and lets
// the test observe requests and control verdicts.
type fakeGate struct {
	t        *testing.T
	verdicts chan byte   // verdict to send per request (test feeds it)
	reqs     chan []byte // received request bodies
	done     chan struct{}
	stop     chan struct{}
}

func newFakeGate(t *testing.T) (*fakeGate, *os.File) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	fg := &fakeGate{t: t, verdicts: make(chan byte, 16), reqs: make(chan []byte, 16), done: make(chan struct{}), stop: make(chan struct{})}
	clientFile := os.NewFile(uintptr(fds[0]), "gate-client-fd")
	peerFile := os.NewFile(uintptr(fds[1]), "gate-peer-fd")
	c, err := net.FileConn(peerFile)
	peerFile.Close()
	if err != nil {
		clientFile.Close()
		t.Fatalf("peer FileConn: %v", err)
	}
	t.Cleanup(func() {
		clientFile.Close()
		close(fg.stop)
		c.Close()
		select {
		case <-fg.done:
		case <-time.After(5 * time.Second):
			t.Error("fake gate did not stop")
		}
	})
	go func() {
		defer close(fg.done)
		defer c.Close()
		for {
			var lenb [4]byte
			if _, err := io.ReadFull(c, lenb[:]); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(lenb[:]))
			if _, err := io.ReadFull(c, body); err != nil {
				return
			}
			select {
			case fg.reqs <- body:
			case <-fg.stop:
				return
			}
			var v byte
			select {
			case verdict, ok := <-fg.verdicts:
				if !ok {
					return
				}
				v = verdict
			case <-fg.stop:
				return
			}
			if _, err := c.Write([]byte{v}); err != nil {
				return
			}
		}
	}()
	return fg, clientFile
}

func (fg *fakeGate) waitRequest() (kind byte, addr [16]byte, port uint16, prefix []byte) {
	fg.t.Helper()
	select {
	case b := <-fg.reqs:
		if len(b) < 20 {
			fg.t.Fatalf("short request body: %d bytes", len(b))
		}
		copy(addr[:], b[2:18])
		return b[1], addr, binary.BigEndian.Uint16(b[18:20]), b[20:]
	case <-fg.done:
		fg.t.Fatal("gate connection closed before a request arrived")
	case <-time.After(5 * time.Second):
		fg.t.Fatal("gate did not receive a request")
	}
	return
}

func newTestClient(t *testing.T) (*fakeGate, *egressGateClient) {
	t.Helper()
	fg, f := newFakeGate(t)
	c, err := newEgressGateClientFile(f)
	if err != nil {
		t.Fatalf("newEgressGateClient: %v", err)
	}
	t.Cleanup(c.Close)
	return fg, c
}

func TestEgressGateClientTCPVerdicts(t *testing.T) {
	dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{1, 2, 3, 4}), Port: 443}
	for _, tc := range []struct {
		verdict byte
		wantErr bool
	}{{egressGateVerdictAllow, false}, {egressGateVerdictNeedMore, true}, {egressGateVerdictDeny, true}, {0xff /* garbage */, true}} {
		fg, c := newTestClient(t)
		res := make(chan tcpip.Error, 1)
		go func() { res <- c.CheckTCP(dst) }()
		kind, addr, port, _ := fg.waitRequest()
		if kind != egressGateKindTCP {
			t.Fatalf("kind = %d, want %d", kind, egressGateKindTCP)
		}
		if port != 443 {
			t.Fatalf("port = %d, want 443", port)
		}
		// IPv4 must be encoded as v4-in-v6.
		want := [16]byte{10: 0xff, 11: 0xff, 12: 1, 13: 2, 14: 3, 15: 4}
		if addr != want {
			t.Fatalf("addr = %v, want %v", addr, want)
		}
		fg.verdicts <- tc.verdict
		err := <-res
		if got := err != nil; got != tc.wantErr {
			t.Fatalf("verdict %d: err = %v, want error: %v", tc.verdict, err, tc.wantErr)
		}
	}
}

func TestEgressGateClientUDPVerdicts(t *testing.T) {
	fg, c := newTestClient(t)
	dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{8, 8, 8, 8}), Port: 53}
	res := make(chan tcpip.Error, 1)
	go func() { res <- c.CheckUDP(dst) }()
	kind, _, port, prefix := fg.waitRequest()
	if kind != egressGateKindUDP || port != 53 || len(prefix) != 0 {
		t.Fatalf("kind=%d port=%d prefix=%d", kind, port, len(prefix))
	}
	fg.verdicts <- egressGateVerdictAllow
	if err := <-res; err != nil {
		t.Fatalf("allow: err = %v", err)
	}
}

func TestEgressGateClientL7(t *testing.T) {
	fg, c := newTestClient(t)
	dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{9, 9, 9, 9}), Port: 80}
	prefix := []byte("GET / HTTP/1.1\r\n\r\n")
	type result struct {
		needMore bool
		err      tcpip.Error
	}
	res := make(chan result, 1)
	go func() {
		nm, err := c.CheckL7(dst, prefix)
		res <- result{nm, err}
	}()
	kind, _, _, gotPrefix := fg.waitRequest()
	if kind != egressGateKindL7 {
		t.Fatalf("kind = %d, want %d", kind, egressGateKindL7)
	}
	if string(gotPrefix) != string(prefix) {
		t.Fatalf("prefix = %q, want %q", gotPrefix, prefix)
	}
	fg.verdicts <- egressGateVerdictNeedMore
	r := <-res
	if !r.needMore || r.err != nil {
		t.Fatalf("needMore verdict: %+v", r)
	}
}

func TestEgressGateClientL7SniffLimit(t *testing.T) {
	_, c := newTestClient(t)
	dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{9, 9, 9, 9}), Port: 80}
	// An over-limit prefix must be denied without any wire exchange.
	nm, err := c.CheckL7(dst, make([]byte, egressGateL7SniffLimit+1))
	if nm || err == nil {
		t.Fatalf("over-limit prefix: needMore=%v err=%v", nm, err)
	}
}

func TestEgressGateClientLoopbackExempt(t *testing.T) {
	fg, c := newTestClient(t)
	// Loopback and the unspecified address never egress: they are allowed
	// locally without any wire exchange.
	for _, a := range []tcpip.Address{
		tcpip.AddrFrom4([4]byte{127, 0, 0, 1}),
		tcpip.AddrFrom4([4]byte{}),
		tcpip.AddrFrom16([16]byte{15: 1}), // ::1
		tcpip.AddrFrom16([16]byte{}),      // ::
		tcpip.AddrFrom16([16]byte{10: 0xff, 11: 0xff, 12: 127, 15: 1}),
	} {
		if err := c.CheckTCP(tcpip.FullAddress{Addr: a, Port: 80}); err != nil {
			t.Fatalf("loopback %v: err = %v", a, err)
		}
		if err := c.CheckUDP(tcpip.FullAddress{Addr: a, Port: 53}); err != nil {
			t.Fatalf("loopback %v: err = %v", a, err)
		}
	}
	select {
	case b := <-fg.reqs:
		t.Fatalf("unexpected wire request for loopback: %v", b)
	default:
	}
}

func TestEgressGateClientFailClosed(t *testing.T) {
	fg, c := newTestClient(t)
	dst := tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{1, 1, 1, 1}), Port: 80}
	// First request: the peer closes without replying. The client must fail
	// closed and never reuse the connection.
	res := make(chan tcpip.Error, 1)
	go func() { res <- c.CheckTCP(dst) }()
	fg.waitRequest()
	close(fg.verdicts)
	if err := <-res; err == nil {
		t.Fatal("flow with no verdict was allowed")
	}
	// Every subsequent flow must be denied without touching the wire.
	if err := c.CheckTCP(dst); err == nil {
		t.Fatal("flow after gate death was allowed")
	}
	if err := c.CheckUDP(dst); err == nil {
		t.Fatal("UDP flow after gate death was allowed")
	}
}

// TestEgressGateConfigRejections verifies the fail-closed contract: --egress-fd
// is rejected for any configuration whose egress would bypass the gate, rather
// than being silently accepted and ignored.
func TestEgressGateConfigRejections(t *testing.T) {
	fd := 3
	cases := []struct {
		name string
		conf *config.Config
		want string
	}{
		{
			name: "host networking",
			conf: &config.Config{Network: config.NetworkHost},
			want: "requires network=sandbox",
		},
		{
			name: "plugin networking",
			conf: &config.Config{Network: config.NetworkPlugin},
			want: "requires network=sandbox",
		},
		{
			name: "raw sockets",
			conf: &config.Config{Network: config.NetworkSandbox, EnableRaw: true},
			want: "--net-raw",
		},
		{
			name: "packet socket writes",
			conf: &config.Config{Network: config.NetworkSandbox, AllowPacketEndpointWrite: true},
			want: "--allow-packet-socket-write",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := newRootNetworkNamespace(tc.conf, nil, nil, nil, &fd)
			if err == nil {
				t.Fatalf("newRootNetworkNamespace accepted --egress-fd with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err, tc.want)
			}
		})
	}
}

// TestEgressGateCreatorAfterLoad covers restore-time gate re-attachment on the
// netstack creator: the live client is pulled from the restore context, and a
// checkpoint taken with a gate but restored without one fails closed instead
// of silently running ungated (or gating on a recycled FD number).
func TestEgressGateCreatorAfterLoad(t *testing.T) {
	fd := 3

	// No gate in the context and no donated gate: a gated checkpoint restored
	// without --egress-fd must be marked, and CreateStack must refuse.
	c := &sandboxNetstackCreator{egressFD: &fd}
	c.afterLoad(stdcontext.Background())
	if !c.egressGateMissing {
		t.Fatal("egressGateMissing not set for gated checkpoint restored without --egress-fd")
	}
	if _, err := c.CreateStack(); err == nil || !strings.Contains(err.Error(), "donate --egress-fd") {
		t.Fatalf("CreateStack error = %v, want egress-fd donation error", err)
	}

	// A gate in the context is adopted by the restored creator.
	injected := &egressGateClient{}
	c2 := &sandboxNetstackCreator{egressFD: &fd}
	c2.afterLoad(stdcontext.WithValue(stdcontext.Background(), stack.CtxEgressGate{}, stack.EgressGate(injected)))
	if c2.egressGateMissing {
		t.Fatal("egressGateMissing set despite donated gate")
	}
	if c2.egressGate != injected {
		t.Fatal("restored creator did not adopt the context gate")
	}
}
