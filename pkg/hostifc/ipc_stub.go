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

package hostifc

import (
	"gvisor.dev/gvisor/pkg/fdchannel"
	"gvisor.dev/gvisor/pkg/flipcall"
)

// unsupportedIPC fails every operation with ErrUnsupported. Non-Linux
// backends must provide a real implementation and wire its callers. These
// stubs support compilation of the interface package on supported non-Linux
// build targets; they do not provide a runnable non-Linux sentry. See
// pkg/sentry/platform/platform-seam.md.
type unsupportedIPC struct{}

func defaultIPCImpl() IPC { return unsupportedIPC{} }

// ControlSocketFromFD implements IPC.ControlSocketFromFD.
func (unsupportedIPC) ControlSocketFromFD(int) (ControlSocket, error) {
	return nil, ErrUnsupported
}

// NewControlSocketPair implements IPC.NewControlSocketPair.
func (unsupportedIPC) NewControlSocketPair() (ControlSocket, ControlSocket, error) {
	return nil, nil, ErrUnsupported
}

// NewFDChannel implements IPC.NewFDChannel.
func (unsupportedIPC) NewFDChannel() (fdchannel.FDDonator, int, error) {
	return nil, -1, ErrUnsupported
}

// NewPacketWindowAllocator implements IPC.NewPacketWindowAllocator.
func (unsupportedIPC) NewPacketWindowAllocator() (flipcall.WindowAllocator, error) {
	return nil, ErrUnsupported
}
