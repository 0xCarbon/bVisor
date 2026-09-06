// Copyright 2018 The gVisor Authors.
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

package platform

import "gvisor.dev/gvisor/pkg/hostarch"

// There is no /proc/sys/vm/mmap_min_addr on non-Linux hosts, and the
// Linux-side implementation panics at init when the file is missing. This
// stub keeps the sentry core loadable on non-Linux hosts: the host minimum
// address is reported as 0, and the MMapMinAddr embed (which exists for
// platforms whose application address space is the host's: systrap, ptrace,
// KVM, all Linux-only) must not be used by non-Linux platform
// implementations. VM-backed platforms (Hypervisor.framework, WHP2) define
// their own MinUserAddress from the guest address space layout instead.
// See platform-seam.md.

// systemMMapMinAddr is the system's minimum map address (0: unavailable).
var systemMMapMinAddr uint64

// SystemMMapMinAddr returns the minimum system address.
func SystemMMapMinAddr() hostarch.Addr {
	return hostarch.Addr(systemMMapMinAddr)
}

// MMapMinAddr is a size zero struct that implements MinUserAddress based on
// the system minimum address. On non-Linux hosts this always reports 0 and
// must not be embedded by platform implementations.
type MMapMinAddr struct {
}

// MinUserAddress implements platform.MinUserAddresss.
func (*MMapMinAddr) MinUserAddress() hostarch.Addr {
	return SystemMMapMinAddr()
}
