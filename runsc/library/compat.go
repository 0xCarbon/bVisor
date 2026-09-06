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

package library

import (
	"fmt"

	"gvisor.dev/gvisor/runsc/compat"
)

// CompatKey returns the restore-compatibility class of this host under the
// runtime's platform: the key a checkpoint image must carry to be
// restorable here. driver optionally pins a GPU driver class; pass "" for
// CPU sandboxes. It is the in-process form of
// `runsc cpu-features --compat-key` (and calls the same compat.HostKey).
//
// The zero-cost workflow: Checkpoint returns the image's key; record it
// with the image metadata; at placement time compare candidates with
// Runtime.CompatKey before committing, or just pass it back via
// RestoreOptions.ExpectedCompatKey and let Restore refuse mismatches with a
// typed *IncompatibleKey.
func (r *Runtime) CompatKey(driver string) compat.Key {
	return compat.HostKey(r.conf.Platform, driver)
}

// checkCompatKey validates a recorded key against this host. It runs before
// any sandbox is built; failures are *IncompatibleKey.
func (r *Runtime) checkCompatKey(expected, driver string) error {
	image, err := compat.Parse(expected)
	if err != nil {
		return fmt.Errorf("library: parsing expected compat key: %w", err)
	}
	host := r.CompatKey(driver)
	if !host.Compatible(image) {
		return &IncompatibleKey{Host: host, Image: image, Expected: expected}
	}
	return nil
}
