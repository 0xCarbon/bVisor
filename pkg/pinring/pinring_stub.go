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

package pinring

import (
	"errors"
	"os"
	"time"
)

// NewDisabledIOURing is unavailable on hosts without Linux io_uring.
func NewDisabledIOURing() (*os.File, error) {
	return nil, errors.ErrUnsupported
}

func registerFiles(ring int, fds []int32) error {
	return errors.ErrUnsupported
}

// SendPidfd is unavailable on hosts without Linux pidfds.
func SendPidfd(sock *os.File, pid int) error {
	return errors.ErrUnsupported
}

// WaitExit cannot confirm process exit without Linux pidfds.
func WaitExit(sockFD int, timeout time.Duration) bool {
	return false
}
