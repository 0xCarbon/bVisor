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

package hostmm

import "golang.org/x/sys/unix"

// membarrier(2) does not exist on non-Linux hosts: Probe reports no support
// (so the barrier methods' preconditions never hold), and the syscall
// helpers fail closed with ENOSYS as unreachable insurance. VM-backed
// platforms (Hypervisor.framework, WHP2) do not need a host membarrier
// because application threads are vCPUs the platform already serializes.
// See pkg/sentry/platform/platform-seam.md.
func membarrierSyscall(uintptr) unix.Errno {
	return unix.ENOSYS
}

func membarrierRawSyscall(uintptr) (uintptr, unix.Errno) {
	return 0, unix.ENOSYS
}

// unixENOSYS is the errno probe treats as "membarrier not implemented".
const unixENOSYS = unix.ENOSYS
