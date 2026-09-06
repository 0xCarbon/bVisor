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

//go:build !linux
// +build !linux

package flipcall

import "runtime"

// Non-Linux hosts have no control-transfer primitive yet: every operation
// that must block or wake a peer fails with ErrUnsupported instead of
// silently corrupting the protocol. Wave-05 backends replace these stubs
// with real Sleeper implementations (see seam.go and SEAM.md). The word
// accessors (connState/dataLen) remain functional so window layout code and
// tests are host-independent.

func (ep *Endpoint) futexSetPeerActive() error {
	return ep.controlUnsupported()
}

func (ep *Endpoint) futexWakePeer() error {
	return ep.controlUnsupported()
}

func (ep *Endpoint) futexWaitUntilActive() error {
	return ep.controlUnsupported()
}

func (ep *Endpoint) futexWakeConnState(numThreads int32) error {
	return ep.controlUnsupported()
}

func (ep *Endpoint) futexWaitConnState(curState uint32) error {
	return ep.controlUnsupported()
}

// controlUnsupported reports that this host provides no control-transfer
// primitive. It still observes the shutdown state so that a local Shutdown()
// is surfaced as ShutdownError rather than a generic failure.
func (ep *Endpoint) controlUnsupported() error {
	if ep.shutdown.Load() != 0 {
		return ShutdownError{}
	}
	return ErrUnsupported
}

// futexSleeper is the (absent) non-Linux Sleeper implementation.
type futexSleeper struct {
	ep *Endpoint
}

// Sleeper implements the seam; on non-Linux hosts it always fails with
// ErrUnsupported (or ShutdownError once shut down).
func (ep *Endpoint) Sleeper() Sleeper { return futexSleeper{ep: ep} }

func (s futexSleeper) Wake(n int32) error    { return s.ep.controlUnsupported() }
func (s futexSleeper) Wait(cur uint32) error { return s.ep.controlUnsupported() }

// yieldThread still yields the goroutine where possible.
func yieldThread() {
	runtime.Gosched()
}
