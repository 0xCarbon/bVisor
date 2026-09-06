// Copyright 2020 The gVisor Authors.
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

//go:build linux
// +build linux

package hostmm

import (
	"golang.org/x/sys/unix"
)

// membarrierSyscall issues the blocking-safe form of membarrier(2) for
// commands that may block (MEMBARRIER_CMD_GLOBAL).
func membarrierSyscall(cmd uintptr) unix.Errno {
	_, _, e := unix.Syscall(unix.SYS_MEMBARRIER, cmd, 0 /* flags */, 0 /* unused */)
	return e
}

// membarrierRawSyscall issues the non-blocking form of membarrier(2) for
// commands that never block, returning the raw return value.
func membarrierRawSyscall(cmd uintptr) (uintptr, unix.Errno) {
	n, _, e := unix.RawSyscall(unix.SYS_MEMBARRIER, cmd, 0 /* flags */, 0 /* unused */)
	return n, e
}

// unixENOSYS is the errno probe treats as "membarrier not implemented".
const unixENOSYS = unix.ENOSYS
