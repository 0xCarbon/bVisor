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

package sandbox

import (
	"errors"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func TestInvalidSignalsRejectedBeforeRPC(t *testing.T) {
	var s Sandbox
	for _, value := range []string{"-1", "65", "2147483648", "4294967305"} {
		t.Run(value, func(t *testing.T) {
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Skip("value is not representable on this architecture")
			}
			sig := unix.Signal(n)
			for name, signal := range map[string]func() error{
				"container": func() error { return s.SignalContainer("test", sig, false) },
				"process":   func() error { return s.SignalProcess("test", 1, sig, false) },
				"group":     func() error { return s.SignalProcessGroup("test", 1, sig) },
			} {
				if err := signal(); !errors.Is(err, unix.EINVAL) {
					t.Errorf("%s signal %d: got %v, want EINVAL before contacting the sandbox", name, sig, err)
				}
			}
		})
	}
}
