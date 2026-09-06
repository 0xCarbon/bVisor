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

package flipcall

import "errors"

// ErrUnsupported is returned by seam operations on hosts that do not provide
// the required primitive. Today that is every non-Linux host: the Linux
// implementation (futex(2) + sealed memfd + SCM_RIGHTS) is the only backend;
// wave-05 backends (POSIX shm / Mach ports / named pipes) implement the seams
// below instead. See SEAM.md.
var ErrUnsupported = errors.New("flipcall: control transfer is not supported on this host")

// Sleeper is the blocking control-transfer seam of flipcall. It operates on
// the shared 32-bit connection-state word at offset 0 of the packet window
// (native endian; see SEAM.md §Packet window header).
//
// Implementations must have futex(2)-like semantics:
//
//   - Wake(n) wakes at most n threads blocked in Wait on the same word; it
//     does not require any thread to be blocked.
//
//   - Wait(cur) blocks while the word's value equals cur. It must return
//     without error on a spurious wake-up or a concurrent word change
//     (futex(2) EAGAIN/EINTR); callers are required to re-check the word in
//     a loop, never to rely on Wait returning only on a real wake.
//
// Wake and Wait must be callable from different threads of the same process
// pair sharing the window mapping.
type Sleeper interface {
	Wake(n int32) error
	Wait(cur uint32) error
}

// WindowAllocator is the seam for allocating packet windows: page-aligned
// regions of a shared memory file that are exchangeable with a peer process
// (by transmitting the file descriptor) and safe against truncation while
// mapped (Linux: memfd sealed F_SEAL_SHRINK|F_SEAL_SEAL; see SEAM.md).
//
// Allocate returns a descriptor whose FD is the shared memory file, Offset
// the page-aligned start of the window within that file, and Length the
// page-rounded window size. Implementations are not required to be safe
// for concurrent use; callers allocate windows during channel setup, before
// the window is shared with a peer. (The current Linux allocator is not
// internally synchronized.)
type WindowAllocator interface {
	// Allocate allocates a new packet window of at least size bytes.
	// Preconditions: size > 0.
	Allocate(size int) (PacketWindowDescriptor, error)

	// FD returns the file descriptor of the backing shared memory file.
	FD() int

	// Destroy releases resources owned by the allocator. It invalidates all
	// descriptors previously returned by Allocate.
	Destroy()
}

// The Linux implementation satisfies the seams unchanged.
var (
	_ WindowAllocator = (*PacketWindowAllocator)(nil)
	_ Sleeper         = futexSleeper{}
)
