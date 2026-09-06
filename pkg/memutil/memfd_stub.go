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

package memutil

import "fmt"

// CreateMemFD creates a memfd file and returns the fd. It is only
// implemented on Linux; non-Linux hosts fail closed (wave-05 backends
// provide the shared-memory primitive behind flipcall's WindowAllocator
// seam; see pkg/flipcall/SEAM.md).
func CreateMemFD(name string, flags int) (int, error) {
	return -1, fmt.Errorf("memfd_create is not supported on this platform")
}
