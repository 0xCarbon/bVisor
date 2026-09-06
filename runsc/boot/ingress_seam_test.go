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
	"strings"
	"testing"

	"gvisor.dev/gvisor/runsc/config"
)

// TestIngressConfigRejections verifies the fail-closed contract of the
// ingress seam: --ingress-fd is rejected for any configuration where the
// relay has no sandbox netstack to serve, rather than being silently
// accepted while the donating host waits forever.
func TestIngressConfigRejections(t *testing.T) {
	fd := 3
	cases := []struct {
		name string
		conf *config.Config
		fd   *int
		want string
	}{
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
			err := validateIngressFD(tc.conf, tc.fd)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateIngressFD(%s) = %v, want nil", tc.name, err)
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
