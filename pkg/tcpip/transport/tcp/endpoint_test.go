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

package tcp

import (
	"bytes"
	"fmt"
	"testing"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestSendBufferStateSnapshot(t *testing.T) {
	var ep Endpoint
	var snapshot TCPSndBufState
	// Neither the initial nor updated endpoint state contains this value.
	snapshot.AutoTuneSndBufDisabled.Store(2)

	// The socket option callback does not take sndQueueMu. Join only after
	// both accesses so that the wait group cannot serialize them.
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = ep.OnSetSendBufferSize(4096)
	})
	wg.Go(func() {
		ep.sndQueueInfo.sndQueueMu.Lock()
		ep.sndQueueInfo.CloneState(&snapshot)
		ep.sndQueueInfo.sndQueueMu.Unlock()
	})
	wg.Wait()

	ep.sndQueueInfo.sndQueueMu.Lock()
	sourceDisabled := ep.sndQueueInfo.AutoTuneSndBufDisabled.Load()
	ep.sndQueueInfo.sndQueueMu.Unlock()
	if sourceDisabled != 1 {
		t.Errorf("source AutoTuneSndBufDisabled = %d, want 1", sourceDisabled)
	}
	if got := snapshot.AutoTuneSndBufDisabled.Load(); got > 1 {
		t.Errorf("snapshot AutoTuneSndBufDisabled = %d, want 0 or 1", got)
	}
}

func TestEgressL7PrefixPreservesPayload(t *testing.T) {
	var ep Endpoint
	ep.mu.Lock()
	defer ep.mu.Unlock()
	var want []byte
	for _, size := range []int{17, 2 * egressL7SniffLimit, 100} {
		payload := bytes.Repeat([]byte{byte(size)}, size)
		want = append(want, payload...)
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(payload)})
		ep.appendEgressL7Prefix(&segment{pkt: pkt})
		gotPayload := pkt.Data().AsRange().ToSlice()
		pkt.DecRef()
		if !bytes.Equal(gotPayload, payload) {
			t.Fatal("capturing the prefix changed the TCP send queue payload")
		}
		if limit := min(len(want), egressL7SniffLimit); !bytes.Equal(ep.egressL7Prefix, want[:limit]) {
			t.Fatalf("captured %d bytes, want the first %d stream bytes", len(ep.egressL7Prefix), limit)
		}
	}
}

// The sniff window is fixed even when a write spans many payload buffers.
// Allocated bytes and capture time should not grow with the entire write.
func BenchmarkEgressL7Prefix(b *testing.B) {
	for _, size := range []int{32 << 10, 1 << 20, 4 << 20} {
		b.Run(fmt.Sprintf("payload_%d", size), func(b *testing.B) {
			var payload buffer.Buffer
			for remaining := size; remaining > 0; remaining -= 4096 {
				if err := payload.Append(buffer.NewViewSize(4096)); err != nil {
					b.Fatal(err)
				}
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: payload})
			defer pkt.DecRef()
			seg := &segment{pkt: pkt}
			var ep Endpoint
			ep.mu.Lock()
			defer ep.mu.Unlock()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ep.egressL7Prefix = nil
				ep.appendEgressL7Prefix(seg)
			}
		})
	}
}
