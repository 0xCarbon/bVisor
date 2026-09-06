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
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"gvisor.dev/gvisor/runsc/config"
)

// TestIngressConfigRejections verifies the fail-closed contract of the
// ingress seam: --ingress-fd is rejected for any configuration where the
// relay has no sandbox netstack to serve, rather than being silently
// accepted while the donating host waits forever.
func TestIngressConfigRejections(t *testing.T) {
	fd, zero, negative := 3, 0, -1
	cases := []struct {
		name string
		conf *config.Config
		fd   *int
		want string
	}{
		{name: "none networking accepts descriptor zero", conf: &config.Config{Network: config.NetworkNone}, fd: &zero},
		{name: "negative donated descriptor", conf: &config.Config{Network: config.NetworkSandbox}, fd: &negative, want: "invalid ingress FD"},
		{name: "unknown network mode", conf: &config.Config{Network: config.NetworkType(99)}, fd: &fd, want: "requires network=sandbox"},
		{
			name: "host networking",
			conf: &config.Config{Network: config.NetworkHost},
			fd:   &fd,
			want: "requires network=sandbox",
		},
		{
			name: "plugin networking",
			conf: &config.Config{Network: config.NetworkPlugin},
			fd:   &fd,
			want: "requires network=sandbox",
		},
		{
			name: "disabled flag accepted everywhere",
			conf: &config.Config{Network: config.NetworkHost},
			fd:   nil,
			want: "",
		},
		{
			name: "sandbox networking accepted",
			conf: &config.Config{Network: config.NetworkSandbox},
			fd:   &fd,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateIngressFD(tc.conf, tc.fd)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateIngressFD(%s) = %v, want nil", tc.name, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateIngressFD accepted --ingress-fd with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestIngressSocketValidation(t *testing.T) {
	for _, kind := range []int{syscall.SOCK_STREAM, syscall.SOCK_DGRAM, syscall.SOCK_SEQPACKET} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			pair, err := syscall.Socketpair(syscall.AF_UNIX, kind, 0)
			if err != nil {
				t.Fatal(err)
			}
			host := os.NewFile(uintptr(pair[0]), "host")
			defer host.Close()
			donated := os.NewFile(uintptr(pair[1]), "ingress")
			relay, err := newIngressRelayFile(donated, nil)
			if kind == syscall.SOCK_STREAM {
				if err != nil {
					t.Fatal(err)
				}
				relay.Close()
			} else if err == nil {
				relay.Close()
				t.Fatal("accepted non-stream socket")
			}
			if _, err := donated.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("donation not consumed: %v", err)
			}
		})
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if relay, err := newIngressRelayFile(r, nil); err == nil {
		relay.Close()
		t.Fatal("accepted pipe as ingress transport")
	}
	if _, err := r.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("invalid donation not closed: %v", err)
	}
}
