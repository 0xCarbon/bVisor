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

package eventfd

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Eventfd is the API shape of a Linux eventfd object. On non-Linux hosts
// eventfds cannot be created: Create fails with errors.ErrUnsupported, and
// every eventfd-semantics operation fails closed the same way. A descriptor
// may still be adopted with Wrap so that FD and Close remain usable by
// descriptor-table bookkeeping; it is not an eventfd on these hosts.
//
// A non-Linux backend (wave-05+; see pkg/sentry/platform/platform-seam.md)
// provides the wake-up primitive eventfd stand-ins need (e.g. a pipe or
// kqueue-notified descriptor on macOS, or the WHP2 equivalent on Windows).
type Eventfd struct {
	fd int
}

// Create returns an error on non-Linux hosts; see Eventfd.
func Create() (Eventfd, error) {
	return Eventfd{}, errors.ErrUnsupported
}

// Wrap returns an Eventfd wrapping the given descriptor.
func Wrap(fd int) Eventfd {
	return Eventfd{fd: fd}
}

// Close implements Eventfd.Close.
func (ev Eventfd) Close() error {
	return unix.Close(ev.fd)
}

// FD implements Eventfd.FD.
func (ev Eventfd) FD() int {
	return ev.fd
}

// Dup implements Eventfd.Dup.
func (ev Eventfd) Dup() (Eventfd, error) {
	other, err := unix.Dup(ev.fd)
	return Eventfd{fd: other}, err
}

// Notify implements Eventfd.Notify.
func (ev Eventfd) Notify() error {
	return errors.ErrUnsupported
}

// Write implements Eventfd.Write.
func (ev Eventfd) Write(uint64) error {
	return errors.ErrUnsupported
}

// MMIOWrite implements Eventfd.MMIOWrite.
func (ev Eventfd) MMIOWrite(uint64) error {
	return errors.ErrUnsupported
}

// Wait implements Eventfd.Wait.
func (ev Eventfd) Wait() error {
	return errors.ErrUnsupported
}

// Read implements Eventfd.Read.
func (ev Eventfd) Read() (uint64, error) {
	return 0, errors.ErrUnsupported
}

// MMIOController is as defined by the Linux implementation; see
// eventfd.go. It is declared here so that importers compile on non-Linux
// hosts.
type MMIOController interface {
	// Enabled returns true if writing to the associated MMIO address can
	// succeed.
	Enabled() bool

	// Close is called when the associated Eventfd is closed.
	Close(ev Eventfd)
}

// EnableMMIO implements Eventfd.EnableMMIO. No MMIO write path exists on
// non-Linux hosts; the request is discarded (kept for API parity).
func (ev *Eventfd) EnableMMIO(addr uintptr, ctrl MMIOController) {
	_ = addr
	_ = ctrl
}

// DisableMMIO implements Eventfd.DisableMMIO.
func (ev *Eventfd) DisableMMIO() {
}

// MMIOAddr implements Eventfd.MMIOAddr.
func (ev Eventfd) MMIOAddr() uintptr {
	return 0
}
