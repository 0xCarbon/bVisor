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
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/socket/netstack"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/runsc/boot/ingress"
	"gvisor.dev/gvisor/runsc/config"
)

const ingressDialTimeout = 5 * time.Second

// ValidateIngressFD checks whether the selected network supports ingress.
// A nil descriptor disables ingress; descriptor zero is a valid donation.
func ValidateIngressFD(conf *config.Config, ingressFD *int) error {
	if ingressFD == nil {
		return nil
	}
	if *ingressFD < 0 {
		return fmt.Errorf("invalid ingress FD %d", *ingressFD)
	}
	switch conf.Network {
	case config.NetworkSandbox, config.NetworkNone:
		return nil
	default:
		return fmt.Errorf("--ingress-fd requires network=sandbox or network=none; %s networking has no sandbox netstack to dial", conf.Network)
	}
}

// newIngressRelay consumes donatedFD. currentKernel must return the Loader's
// current kernel: restore replaces the kernel that was constructed by New.
func newIngressRelay(donatedFD int, currentKernel func() *kernel.Kernel) (*ingress.Relay, error) {
	f := os.NewFile(uintptr(donatedFD), "ingress-fd")
	if f == nil {
		return nil, fmt.Errorf("ingress: invalid fd %d", donatedFD)
	}
	return newIngressRelayFile(f, currentKernel)
}

// newIngressRelayFile consumes f, including on error. Keep the same os.File
// owner when this descriptor comes from a URPC FilePayload.
func newIngressRelayFile(f *os.File, currentKernel func() *kernel.Kernel) (*ingress.Relay, error) {
	defer f.Close()
	conn, err := net.FileConn(f)
	if err != nil {
		return nil, fmt.Errorf("ingress: FileConn: %w", err)
	}
	if _, ok := conn.(*net.UnixConn); !ok || conn.LocalAddr().Network() != "unix" || conn.RemoteAddr() == nil {
		conn.Close()
		return nil, fmt.Errorf("ingress: FD must be a connected Unix stream socket")
	}
	return ingress.NewRelay(conn, ingressNetstackDialer(currentKernel)), nil
}

func ingressNetstackDialer(currentKernel func() *kernel.Kernel) ingress.Dialer {
	return func(ctx context.Context, target [16]byte, port uint16) (net.Conn, error) {
		k := currentKernel()
		if k == nil || k.RootNetworkNamespace() == nil {
			return nil, fmt.Errorf("ingress: no root network namespace")
		}
		s, ok := k.RootNetworkNamespace().Stack().(*netstack.Stack)
		if !ok {
			return nil, fmt.Errorf("ingress: no sandbox netstack")
		}
		addr := tcpip.AddrFrom16(target)
		network := ipv6.ProtocolNumber
		if v4 := addr.To4(); v4.Len() == 4 {
			addr, network = v4, ipv4.ProtocolNumber
		}
		ctx, cancel := context.WithTimeout(ctx, ingressDialTimeout)
		defer cancel()
		conn, err := gonet.DialContextTCP(ctx, s.Stack, tcpip.FullAddress{Addr: addr, Port: port}, network)
		if err != nil {
			// Do not box a nil *gonet.TCPConn into a non-nil net.Conn.
			return nil, err
		}
		return conn, nil
	}
}
