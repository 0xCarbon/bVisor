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

package pinring

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// SendPidfd sends a `pidfd` of process `pid` over `sock`.
// See `WaitExit` below.
func SendPidfd(sock *os.File, pid int) error {
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("pidfd_open(%d): %w", pid, err)
	}
	defer unix.Close(pidfd)
	return unix.Sendmsg(int(sock.Fd()), []byte{0}, unix.UnixRights(pidfd), nil, 0)
}

// WaitExit blocks until the process whose `pidfd` arrives over socket `sockFD`
// (sent by `SendPidfd`) has fully exited, and reports false only if it is
// confirmed still running after `timeout`.
func WaitExit(sockFD int, timeout time.Duration) bool {
	pollIn := func(fd int32, timeout time.Duration) bool {
		ts := unix.NsecToTimespec(timeout.Nanoseconds())
		for {
			n, err := unix.Ppoll([]unix.PollFd{{Fd: fd, Events: unix.POLLIN}}, &ts, nil)
			if err == unix.EINTR {
				continue
			}
			return err == nil && n > 0
		}
	}
	// Wait for the pidfd to come in.
	if !pollIn(int32(sockFD), timeout) {
		return true
	}
	// The flags must be exactly what the gofer's seccomp filter allows for `recvmsg`.
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := unix.Recvmsg(sockFD, buf, oob, unix.MSG_DONTWAIT|unix.MSG_TRUNC)
	if err != nil {
		return true
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return true
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) == 0 {
		return true
	}
	defer unix.Close(fds[0])
	// Wait for the process to die.
	return pollIn(int32(fds[0]), timeout)
}
