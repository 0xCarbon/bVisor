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

package sandbox

import (
	"errors"
	"io"
	"strings"
	"syscall"
)

// Checkpoint failure taxonomy (wave-04).
//
// The urpc control channel flattens sandbox-side errors to their Error()
// strings, so embedders used to substring-match stderr text to tell
// checkpoint failure classes apart. This file types those classes on the
// runsc library side: Sandbox.Checkpoint wraps its errors so consumers can
// use errors.As / the Is* helpers below instead of matching text.
//
// Save-time network policy in this tree (the "--net-disconnect-ok" story):
//
//   - `runsc checkpoint` (resume=false) reconfigures the network stack for
//     restore: NICs and routes are dropped at save (Stack.beforeSave with
//     removeConf) and recreated from the restore-time network configuration.
//     Connected TCP endpoints are RESET at save unless
//     --allow-connected-on-save is set AND the route has save/restore
//     capability. With resume=true, connected endpoints are terminated at
//     restore instead.
//   - --net-disconnect-ok is accepted for compatibility but only consumed by
//     XDP-tagged builds (runsc/sandbox/xdp.go), which are not compiled by
//     default; in default builds the flag has no effect. The behavior gap it
//     used to cover is --allow-connected-on-save (reset vs preserve
//     connected endpoints across a save).
//   - tcpip.ErrSaveRejection is the sentry-side type for "the sandbox
//     refused to save unsupported state". No netstack path currently
//     produces it, but its Error() text is the stable wire contract this
//     package classifies; new rejection sites must keep that prefix.

// saveRejectionPrefix mirrors tcpip.ErrSaveRejection.Error()'s prefix. It is
// cross-checked against the real type in the tests so the two cannot drift.
const saveRejectionPrefix = "save rejected due to unsupported networking state"

// SaveRejection indicates the sandbox refused to checkpoint because it
// reached state that cannot be serialized (the tcpip.ErrSaveRejection
// class). Such a sandbox is generally still kill-checkpointable by the
// consumer; it is not a runsc defect.
type SaveRejection struct {
	// Err is the original flattened error (typically the urpc transport
	// error carrying the sentry's rejection text); preserved for
	// errors.Is/As and string consumers of the original message.
	Err error
}

// Error implements error. The original error already contains the
// saveRejectionPrefix (it is the match condition), so return it verbatim
// rather than re-prefixing.
func (e *SaveRejection) Error() string {
	return e.Err.Error()
}

// Unwrap allows errors.Is/As to reach the original transport error.
func (e *SaveRejection) Unwrap() error { return e.Err }

// SandboxDeath indicates the sandbox process died before the checkpoint RPC
// completed (for example an unexpected sentry exit mid-save), so the image,
// if any, was never finalized. Since the fix for the checkpoint-image
// truncation bug, a failed checkpoint also removes the image files it
// created, so this error means "no image exists".
type SandboxDeath struct {
	// Err is the underlying (urpc transport) error.
	Err error
}

// Error implements error.
func (e *SandboxDeath) Error() string {
	return "sandbox died during checkpoint: " + e.Err.Error()
}

// Unwrap allows errors.Is/As to reach the underlying transport error.
func (e *SandboxDeath) Unwrap() error { return e.Err }

// IsSaveRejection reports whether err is (or wraps) a SaveRejection: the
// sandbox refused to save unsupported state. Consumers use this to decide
// fallback strategies (e.g. oca falls back to its kill-checkpoint tier)
// instead of matching error text.
func IsSaveRejection(err error) bool {
	var sr *SaveRejection
	return errors.As(err, &sr)
}

// IsSandboxDeath reports whether err is (or wraps) a SandboxDeath: the
// sandbox died before the checkpoint RPC completed and no image exists.
func IsSandboxDeath(err error) bool {
	var sd *SandboxDeath
	return errors.As(err, &sd)
}

// classifyCheckpointError types the flattened error strings that cross the
// checkpoint RPC boundary. It preserves the original error in the wrap chain
// and returns err unchanged when no class matches.
func classifyCheckpointError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// The rejection text may be embedded in the flattened urpc error message
	// ("urpc method ... failed: <sentry error text>"), so locate it anywhere.
	if idx := strings.Index(msg, saveRejectionPrefix+":"); idx >= 0 {
		return &SaveRejection{Err: err}
	}
	// urpc flattens transport errors to text; recognize the two signatures of
	// a control connection to a dead sandbox.
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) ||
		strings.HasSuffix(msg, "failed: EOF") || strings.HasSuffix(msg, "failed: connection reset by peer") ||
		// Connect-time death: the sandbox exited before sandboxConnect ran.
		// The dial itself fails: connError's "connecting to control
		// server at PID %d: %v" (ECONNREFUSED once the killed listener's
		// socket is gone), a missing control-socket path, or an
		// unopenable socket file. The shapes are pinned to the producers
		// in the tests so they cannot drift.
		strings.Contains(msg, "no control socket found") ||
		strings.Contains(msg, "connecting to control server") ||
		strings.Contains(msg, "failed to open socket at") ||
		strings.Contains(msg, "connecting to sandbox: connection refused") {
		return &SandboxDeath{Err: err}
	}
	return err
}
