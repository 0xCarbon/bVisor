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

package fdchannel

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// socketpairSeqPacket returns a connected pair of AF_UNIX SOCK_SEQPACKET
// sockets with FD_CLOEXEC set on both descriptors. Hosts without
// SOCK_CLOEXEC (e.g. macOS: x/sys/unix does not define the constant) cannot
// set the flag atomically at creation time, so it is set afterwards with
// fcntl(2); the window before fcntl returns is acceptable for fdchannel
// because the peer endpoint is only created by an explicit call to
// NewEndpoint with the donated descriptor.
func socketpairSeqPacket() ([2]int, error) {
	// The non-atomic socketpair + fcntl(FD_CLOEXEC) sequence must be
	// guarded against a concurrent fork/exec, or the child could inherit
	// the donation sockets (syscall.ForkLock is the Go runtime's contract
	// for exactly this pattern).
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		return fds, err
	}
	for _, fd := range fds {
		// F_SETFD/FD_CLOEXEC are POSIX and defined on all supported hosts.
		if _, _, e := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); e != 0 {
			unix.Close(fds[0])
			unix.Close(fds[1])
			return [2]int{-1, -1}, e
		}
	}
	return fds, nil
}
