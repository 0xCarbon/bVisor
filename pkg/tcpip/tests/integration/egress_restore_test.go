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

package egressgate_test

import (
	"bytes"
	"context"
	"testing"

	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func restoreGateStack(t *testing.T, original *stack.Stack, gate stack.EgressGate) *stack.Stack {
	t.Helper()
	var image bytes.Buffer
	if _, err := state.Save(context.Background(), &image, original); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	ctx := context.Background()
	if gate != nil {
		ctx = context.WithValue(ctx, stack.CtxEgressGate{}, gate)
	}
	restored := stack.New(stack.Options{})
	t.Cleanup(restored.Destroy)
	if _, err := state.Load(ctx, &image, restored); err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	return restored
}

func TestEgressGateRestoreRetainsEnforcement(t *testing.T) {
	original := stack.New(stack.Options{})
	t.Cleanup(original.Destroy)
	gate := &mockGate{}
	restored := restoreGateStack(t, original, gate)
	if restored.EgressGate() != gate {
		t.Fatal("restore did not install the donated gate")
	}
	if !restored.EgressGated() {
		t.Error("adding a gate at restore did not record the enforcement requirement")
	}
	// Save the restored stack again. The second restore must remember that
	// enforcement was added, even though the original checkpoint was ungated.
	again := restoreGateStack(t, restored, nil)
	assertMissingGateDenies(t, again)
}

func TestEgressGateRestoreWithoutDonation(t *testing.T) {
	original := stack.New(stack.Options{EgressGate: &mockGate{}})
	t.Cleanup(original.Destroy)
	restored := restoreGateStack(t, original, nil)
	assertMissingGateDenies(t, restored)
}

func assertMissingGateDenies(t *testing.T, s *stack.Stack) {
	t.Helper()
	if !s.EgressGated() {
		t.Error("restored stack lost its enforcement requirement")
	}
	g := s.EgressGate()
	if g == nil {
		t.Fatal("restored stack permits traffic without the required gate")
	}
	dst := tcpip.FullAddress{Addr: serverAddr, Port: 443}
	if err := g.CheckTCP(dst); err == nil {
		t.Error("restored gate permits TCP without a donation")
	}
	if err := g.CheckUDP(dst); err == nil {
		t.Error("restored gate permits UDP without a donation")
	}
	if more, err := g.CheckL7(dst, []byte("GET /")); more || err == nil {
		t.Errorf("restored gate CheckL7 = %v, %v; want a denial", more, err)
	}
}
