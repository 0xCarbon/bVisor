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

// Package library is the embeddable, argv-free API for driving the full
// runsc container lifecycle in-process (wave-06).
//
// # Why this package exists
//
// Consumers that manage sandboxes at scale (oca is the reference embedder)
// historically shelled out to the runsc binary: an argv driver of ~1200 lines
// that must serialize options into flags and parse JSON or stderr text back
// out. Two things were structurally impossible over that boundary:
//
//   - Typed errors. runsc/sandbox classifies checkpoint failures into
//     SaveRejection and SandboxDeath (wave-04), but across a process boundary
//     only their Error() strings arrive. errors.As cannot reach through an
//     exec. In-process, the types flow: use errors.As(err, &sandboxDeath) or
//     the IsSaveRejection/IsSandboxDeath aliases below.
//   - Compat keys. runsc/compat.Key is the canonical restore-compatibility
//     class of a checkpoint image (v1|build|platform|cpu-features-id|driver,
//     wave-04). In-process, Runtime.CompatKey computes it for this host and
//     Container.Checkpoint returns the key of the image it just wrote; the
//     embedder stores it with placement metadata and checks it before
//     Restore via RestoreOptions.ExpectedCompatKey (a typed
//     IncompatibleKey error replaces text matching).
//
// The package is implemented ON TOP of runsc/container.Container and
// runsc/sandbox.Sandbox — the exact types the runsc CLI drives. It adds no
// second implementation of lifecycle logic: it is a stable, documented,
// in-process surface over the same code path, plus argv-free configuration
// (config.Config built from flag-registration defaults, exactly like
// testutil.TestConfig, then validated).
//
// # Lifecycle
//
// Build //:release and keep its runsc and gvisor-bin/ directory together.
// Standalone embedders set Options.ExePath to that runsc binary. Sidecars
// are required by the default configuration and must match the release.
//
//	rt, err := library.New(library.Options{Root: "/run/rt", Platform: "systrap"})
//	c, err := rt.Create(library.CreateOptions{ID: "web0", Spec: spec, BundleDir: dir})
//	err = c.Start()
//	res, err := c.Checkpoint(library.CheckpointOptions{ImagePath: img}) // res.CompatKey
//	err = c.Destroy()
//	c2, err := rt.Restore(library.RestoreOptions{ID: "web0-r1", ImagePath: img,
//		Spec: spec, BundleDir: dir,
//		ExpectedCompatKey: res.CompatKey.String()})
//	ws, err := c2.Wait()
//	err = c2.Destroy()
//
// Containers created here are visible to the runsc CLI operating on the same
// --root directory and vice versa: both are front ends over the same
// state-file layout. Runtime.Load adopts a container created earlier (by this
// process or by a CLI invocation).
//
// # FD donation
//
// CreateOptions/RestoreOptions carry the wave-02 donation surface as
// *os.File values instead of raw descriptor numbers:
//
//	GoferIOFiles: sandbox-side LISAFS connections of an external gofer,
//	  one per gofer-backed mount in spec order (root first). When set,
//	  runsc spawns no gofer of its own.
//	EgressFile:  AF_UNIX connection to the egress flow gate re-donated
//	  into the sandbox's netstack.
//	IngressFile: AF_UNIX connection to the host-ingress relay re-donated
//	  into the sandbox's netstack (Oca #541).
//	PassFiles:   host files bound to guest descriptor numbers (stdio and
//	  friends); across checkpoint/restore these are re-donated: a restored
//	  container re-binds saved guest descriptors to whatever the restore
//	  call provides under the same number (and container-name annotation).
//	ExecFile:    host executable file for the container init process.
//
// OWNERSHIP: IngressFile is consumed only on success; on error it remains
// caller-owned. The runtime closes its own duplicate on every return. Other
// donated files retain the legacy ownership contract: the library dups each descriptor
// and closes the caller's *os.File (disarming its finalizer) at Create or
// Restore time; the runtime then donates the dup and closes it
// (donation.DonateAndClose for gofer/egress connections) or transfers it
// (DonateAndTransferCustomFiles for PassFiles) once creation succeeds.
// Callers must not use or close the files after passing them (the library
// already did). On failure before the sandbox spawn, the dup is closed by
// the library and the caller's file was already closed — the descriptor is
// deterministically released either way.
//
// # Concurrency
//
// A Container handle, like the underlying container.Container, is not safe
// for concurrent use. A Runtime is immutable after New and shareable. Use
// Signal or Destroy from other goroutines rather than racing lifecycle calls.
//
// # Context
//
// The API deliberately takes no context.Context. Every operation maps to
// one synchronous RPC into the sandbox process (or a lifecycle state
// change) that cannot be unwound mid-flight: "cancelling" a checkpoint or
// restore that is already executing leaves a half-written image or a
// half-built sandbox, not a clean abort. Cancellation therefore cannot make
// a call safer; it can only orphan state. Embedders that must abandon a
// stuck call use Signal or Destroy from another goroutine (the supported
// interruptions) and observe the resulting failure through the original
// call.
//
// # The CLI boundary
//
// The runsc CLI is NOT rebuilt on top of this package: cmd.Create/cmd.Restore
// already are thin adapters from argv + flags into container.New /
// Container.Restore, and duplicating that hop through a second wrapper adds
// no behavior. The contract between the two front ends is the shared
// container/container + sandbox layer plus the state-file format; this
// package documents and freezes the embedder view of it. New embedder-visible
// behavior lands in container/sandbox and is exposed here and (when it makes
// sense as a flag) in cmd — never in only one of them.
package library
