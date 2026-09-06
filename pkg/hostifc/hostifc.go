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

// Package hostifc is the seam between the sentry and the host operating
// system's local-IPC and control-plane primitives.
//
// The sentry's fast paths (lisafs control sockets and channels, flipcall)
// are built on host primitives that only exist in this exact shape on Linux:
// AF_UNIX sockets with SCM_RIGHTS descriptor donation, SOCK_SEQPACKET
// socketpairs, eventfds, sealed memfds and futexes. This package specifies
// the narrow factory and stream interfaces behind which those primitives
// hide, so that non-Linux hosts (macOS Virtualization.framework, Windows
// WHP2) can provide backends without sentry-wide ifdefs, while the Linux
// implementation stays exactly as it is today (see pkg/flipcall/SEAM.md for
// the wave-03 flipcall seams this builds on, and
// pkg/sentry/platform/platform-seam.md for the full platform mapping).
//
// Fail-closed contract: on a host without a backend, every seam operation
// returns an error wrapping ErrUnsupported rather than panicking or
// silently degrading. Consumers treat these seams as capabilities: probe
// first (ProbeControlPlane) or handle ErrUnsupported.
package hostifc

import (
	"errors"

	"gvisor.dev/gvisor/pkg/fdchannel"
	"gvisor.dev/gvisor/pkg/flipcall"
)

// ErrUnsupported reports that this host does not implement the requested
// host-interface operation. It is errors.ErrUnsupported; stub backends
// return it directly so errors.Is(err, errors.ErrUnsupported) matches every
// fail-closed path in the sentry.
var ErrUnsupported = errors.ErrUnsupported

// FDControl is the ancillary-data descriptor-donation surface shared by
// stream readers and writers (Linux: SCM_RIGHTS control messages on an
// AF_UNIX stream socket).
//
// The contract follows pkg/unet.ControlMessage, which the Linux backend
// satisfies structurally: PackFDs/UnpackFDs arm the ancillary buffer used by
// the next WriteVec; EnableFDs arms reception of at least count descriptors
// on the next ReadVec (an implementation may deliver more); ExtractFDs
// returns received descriptors, transferring ownership to the caller; and
// CloseFDs closes received descriptors that the caller does not want.
// Exactly one of ExtractFDs or CloseFDs should follow each ReadVec that had
// FDs enabled.
type FDControl interface {
	// PackFDs packs the given descriptors for donation on the next write.
	PackFDs(fds ...int)
	// UnpackFDs clears packed descriptors without donating them.
	UnpackFDs()
	// EnableFDs enables receiving at least count descriptors on the next read.
	EnableFDs(count int)
	// ExtractFDs returns descriptors received on the last read.
	ExtractFDs() ([]int, error)
	// CloseFDs closes any received descriptors without returning them.
	CloseFDs()
}

// StreamReader reads from a host-local stream, optionally receiving donated
// descriptors.
type StreamReader interface {
	FDControl
	// ReadVec reads into the given buffers, returning the number of bytes
	// read. A stream read may return fewer bytes than requested with a nil
	// error. Callers must assemble complete protocol messages and collect or
	// close received descriptors after each read, including a failed read.
	ReadVec(dsts [][]byte) (int, error)
}

// StreamWriter writes to a host-local stream, optionally donating packed
// descriptors.
type StreamWriter interface {
	FDControl
	// WriteVec writes from the given buffers, returning the number of bytes
	// written. A stream write may be partial with a nil error. Callers must
	// advance the buffers before retrying and clear packed descriptors after
	// a successful partial write so the retry does not donate them again.
	WriteVec(srcs [][]byte) (int, error)
}

// ControlSocket is a connected host-local ordered byte stream with in-band
// descriptor donation: the transport of a lisafs control socket (the socket
// that carries Mount, Channel and other setup RPCs before fast channels
// exist; see pkg/lisafs/sock.go).
type ControlSocket interface {
	// FD returns the underlying host descriptor. Descriptor-number use
	// breaks the seam's abstraction and is for launcher handoff bookkeeping
	// only.
	FD() int

	// Close closes the socket, releasing its descriptor.
	Close() error

	// Shutdown shuts the connection down, causing concurrent and future
	// reads and writes on both ends to unblock and fail.
	Shutdown() error

	// Reader returns a StreamReader for the socket.
	Reader(blocking bool) StreamReader

	// Writer returns a StreamWriter for the socket.
	Writer(blocking bool) StreamWriter
}

// IPC is the host local-IPC backend: everything a lisafs communication stack
// needs from the host before it can run — the control socket, the per-channel
// descriptor-donation pair, and the shared-memory packet-window allocator
// (see pkg/flipcall/SEAM.md for the latter two).
//
// This interface describes the backend needed by a future host port. The
// current lisafs call sites still use unet/fdchannel/flipcall constructors
// directly; neither Linux nor non-Linux builds route them through Default.
// Providing another IPC implementation also requires wiring those call sites
// and implementing the remaining host dependencies.
type IPC interface {
	// ControlSocketFromFD adopts an already-connected control socket
	// descriptor, usually passed by the sandbox launcher, and returns it as
	// a ControlSocket. Ownership transfers on success; on error the caller
	// retains fd. The Linux adapter may set fd nonblocking before failing.
	ControlSocketFromFD(fd int) (ControlSocket, error)

	// NewControlSocketPair returns a pair of connected control sockets
	// (hosting in-process endpoints, e.g. for tests and self-hosted gofers).
	NewControlSocketPair() (a, b ControlSocket, err error)

	// NewFDChannel returns a connected descriptor-donation channel pair of
	// the flipcall/lisafs channel shape: local stays in this process and
	// satisfies fdchannel.FDDonator semantics (one FD per message, in
	// order); peerFD is the raw descriptor to donate to the peer process
	// over a control socket.
	NewFDChannel() (local fdchannel.FDDonator, peerFD int, err error)

	// NewPacketWindowAllocator returns a shared-memory packet-window
	// allocator for flipcall endpoints. Windows must be page-aligned,
	// disjoint, backed by one descriptor transmissible to the peer, and
	// protected against truncation while mapped (Linux: sealed memfd; see
	// flipcall.WindowAllocator and SEAM.md).
	NewPacketWindowAllocator() (flipcall.WindowAllocator, error)
}

// Default returns the host's IPC backend. On Linux it is the existing
// implementation: AF_UNIX sockets (pkg/unet) for control sockets,
// SCM_RIGHTS on SOCK_SEQPACKET socketpairs (pkg/fdchannel) for FD channels,
// and sealed memfds (pkg/flipcall) for packet windows. On a host without a
// backend, every operation of the returned IPC fails with ErrUnsupported.
func Default() IPC {
	return defaultIPCImpl()
}
