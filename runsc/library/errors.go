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
	"errors"
	"fmt"

	"gvisor.dev/gvisor/runsc/compat"
	"gvisor.dev/gvisor/runsc/sandbox"
)

// Checkpoint failure taxonomy, re-exported so embedders need only this
// package. The types are aliases: errors.As against *library.SaveRejection
// matches the very errors runsc/sandbox produces (and vice versa), and the
// classification lives in one place (sandbox.Checkpoint), not one per
// front end.
//
// These classes only exist in-process. Across an exec boundary (the argv
// driver this package replaces) their Error() strings arrive flattened,
// which is why embedders used to substring-match stderr; see
// runsc/sandbox/checkpoint_errors.go for the full story.

// SaveRejection indicates the sandbox refused to checkpoint because it
// reached state that cannot be serialized. Such a sandbox is generally
// still kill-checkpointable by the consumer; it is not a runsc defect.
type SaveRejection = sandbox.SaveRejection

// SandboxDeath indicates the sandbox process died before the checkpoint RPC
// completed, so no image exists (a failed checkpoint also removes the
// partial image files it created).
type SandboxDeath = sandbox.SandboxDeath

// IsSaveRejection reports whether err is (or wraps) a SaveRejection.
func IsSaveRejection(err error) bool { return sandbox.IsSaveRejection(err) }

// IsSandboxDeath reports whether err is (or wraps) a SandboxDeath.
func IsSandboxDeath(err error) bool { return sandbox.IsSandboxDeath(err) }

// IncompatibleKey is returned by Runtime.Restore and Container.Restore when
// ExpectedCompatKey does not match what this host can restore: the check
// runs BEFORE any sandbox process is built, so a mismatch costs one
// comparison, not a sandbox boot. This is the typed replacement for the
// embedder-side "compare runsc --version and cpu annotations" dance.
type IncompatibleKey struct {
	// Host is the compatibility key this host would restore under
	// (Runtime.CompatKey).
	Host compat.Key

	// Image is the key the checkpoint image was recorded with
	// (CheckpointResult.CompatKey at save time).
	Image compat.Key

	// Expected is the ExpectedCompatKey string as passed (preserved
	// verbatim for diagnostics).
	Expected string
}

// Error implements error.
func (e *IncompatibleKey) Error() string {
	return fmt.Sprintf("checkpoint image is not restorable on this host: image key %q != host key %q (expected %q)", e.Image, e.Host, e.Expected)
}

// IsIncompatibleKey reports whether err is (or wraps) an *IncompatibleKey.
func IsIncompatibleKey(err error) bool {
	var ik *IncompatibleKey
	return errors.As(err, &ik)
}
