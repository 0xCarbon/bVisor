// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package ingress

import (
	"math"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// peerGone observes peer closure even behind unread stream data.
func peerGone(conn net.Conn) bool {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return true
	}
	gone := false
	if err := raw.Control(func(fd uintptr) {
		if fd > math.MaxInt32 {
			gone = true
			return
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLRDHUP}}
		if _, err := unix.Poll(fds, 0); err == nil {
			gone = fds[0].Revents&(unix.POLLRDHUP|unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0
		}
	}); err != nil {
		return true
	}
	return gone
}
