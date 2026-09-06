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

package seccomp

import (
	"errors"

	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/bpf"
)

// Host seccomp filters are a Linux-only control plane: there is no prctl(2)
// PR_SET_SECCOMP equivalent on macOS or Windows. Building and inspecting BPF
// programs (Build, DecodeInstructions, ...) stays available on every host so
// that filter configuration code compiles; installing them fails closed with
// errors.ErrUnsupported. A non-Linux sandbox must obtain its syscall policy
// from the VM boundary instead (see pkg/sentry/platform/platform-seam.md).

// SetFilter fails with errors.ErrUnsupported on non-Linux hosts.
func SetFilter(instrs []bpf.Instruction) error {
	_ = instrs
	return errors.ErrUnsupported
}

// SetFilterInChild returns ENOSYS on non-Linux hosts.
func SetFilterInChild(instrs []bpf.Instruction) unix.Errno {
	_ = instrs
	return unix.ENOSYS
}

// isKillProcessAvailable reports that RET_KILL_PROCESS cannot be probed on
// non-Linux hosts.
func isKillProcessAvailable() (bool, error) {
	return false, nil
}
