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
	"os"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/compat"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/sandbox"
)

// Container is a handle to one container managed by a Runtime. It is the
// embedder-facing view of container.Container; the zero value is not usable,
// handles come from Runtime.Create/Restore/Load.
//
// A Container is NOT safe for concurrent use (like container.Container and
// unlike Runtime). The one supported cross-goroutine pattern is signaling or
// destroying a container whose Wait is blocked in another goroutine. Call
// Destroy exactly once when the container is no longer needed; it is also the
// error-path cleanup for Create/Restore failures that returned a handle.
type Container struct {
	rt   *Runtime
	cont *container.Container
}

// ID returns the container's unique identifier.
func (c *Container) ID() string { return c.cont.ID }

// Spec returns the OCI spec the container runs. The spec is shared with the
// underlying container; callers must not mutate it.
func (c *Container) Spec() *specs.Spec { return c.cont.Spec }

// SandboxPid returns the PID of the sandbox (sentry) process, or -1 if it is
// not running. It is the embedder's liveness probe for the sandbox itself.
func (c *Container) SandboxPid() int { return c.cont.SandboxPid() }

// Start starts a created container: the sandbox process (if not already
// running) is spawned and the container's init process executed. It mirrors
// `runsc start`.
func (c *Container) Start() error {
	if err := c.cont.Start(c.rt.conf); err != nil {
		return fmt.Errorf("library: starting container %q: %w", c.cont.ID, err)
	}
	return nil
}

// CheckpointOptions are the arguments for Container.Checkpoint: the library
// view of the `runsc checkpoint` flags.
type CheckpointOptions struct {
	// ImagePath is the directory the checkpoint image is written to
	// (required). It is created if missing, exactly like the CLI.
	ImagePath string

	// Compression selects image compression: statefile.CompressionLevelNone
	// (the default; also the zero value) or CompressionLevelFlateBestSpeed.
	// Restore-side Background loading requires an uncompressed image.
	Compression statefile.CompressionLevel

	// Resume leaves the container running after the checkpoint completes
	// (`runsc checkpoint --leave-running`). Without it the container is
	// stopped when Checkpoint returns.
	Resume bool

	// Direct writes the checkpoint pages file with O_DIRECT.
	Direct bool

	// ExcludeCommittedZeroPages drops committed zero-filled pages from the
	// image.
	ExcludeCommittedZeroPages bool

	// CudaCheckpointPath/CudaCheckpointSequential configure GPU (nvproxy)
	// checkpointing; see the runsc checkpoint flags of the same names.
	CudaCheckpointPath       string
	CudaCheckpointSequential bool

	// SplitFSCheckpoint is retained for source compatibility.
	// Deprecated: upstream removed split filesystem checkpoints. Setting
	// this field returns an error before writing any checkpoint files.
	SplitFSCheckpoint bool

	// SaveRestoreExecArgv/SaveRestoreExecTimeout configure the optional
	// in-sandbox save/restore hook binary; see the runsc flags.
	SaveRestoreExecArgv    string
	SaveRestoreExecTimeout time.Duration

	// Driver is the GPU driver class to record in the result's compatibility
	// key (empty for CPU sandboxes). It only labels CheckpointResult.
	// CompatKey; it does not change what is saved.
	Driver string
}

// CheckpointResult reports what Container.Checkpoint did.
type CheckpointResult struct {
	// CompatKey is the restore-compatibility class (runsc/compat.Key) of
	// the image just written: this runsc build, this host's CPU features,
	// the runtime's platform and opts.Driver. Store it (serialized via
	// String()) with the image's placement metadata and pass it back as
	// RestoreOptions.ExpectedCompatKey to get a typed, pre-sandbox
	// compatibility check at restore time.
	CompatKey compat.Key
}

// Checkpoint checkpoints the container's sandbox to opts.ImagePath and
// returns the compatibility key of the image. The RPC is synchronous: when
// it returns without error the image is complete (a failed checkpoint also
// removes the partial image files it created).
//
// Failure classes are typed: the returned error wraps *sandbox.SaveRejection
// (the sandbox refused to save unsupported state; it may still be killable)
// or *sandbox.SandboxDeath (the sandbox died; no image exists) — see the
// IsSaveRejection/IsSandboxDeath aliases. errors.As works because this call
// is in-process.
func (c *Container) Checkpoint(opts CheckpointOptions) (*CheckpointResult, error) {
	if opts.SplitFSCheckpoint {
		return nil, fmt.Errorf("library: split filesystem checkpoint is no longer supported")
	}
	if opts.ImagePath == "" {
		return nil, fmt.Errorf("library: checkpointing container %q: ImagePath is required", c.cont.ID)
	}
	// Match the CLI: the image directory is created (not the image files;
	// those are O_EXCL-created by the sandbox so a failed save cleans up
	// after itself without touching pre-existing images).
	if err := os.MkdirAll(opts.ImagePath, 0o755); err != nil {
		return nil, fmt.Errorf("library: checkpointing container %q: creating image path %q: %w", c.cont.ID, opts.ImagePath, err)
	}
	compression := opts.Compression
	if compression == "" {
		compression = statefile.CompressionLevelDefault
	}
	sopts := sandbox.CheckpointOpts{
		Compression:                compression,
		Resume:                     opts.Resume,
		Direct:                     opts.Direct,
		ExcludeCommittedZeroPages:  opts.ExcludeCommittedZeroPages,
		CudaCheckpointPath:         opts.CudaCheckpointPath,
		CudaCheckpointSequential:   opts.CudaCheckpointSequential,
		SaveRestoreExecArgv:        opts.SaveRestoreExecArgv,
		SaveRestoreExecTimeout:     opts.SaveRestoreExecTimeout,
		SaveRestoreExecContainerID: c.cont.ID,
	}
	if err := c.cont.Checkpoint(c.rt.conf, opts.ImagePath, sopts); err != nil {
		return nil, fmt.Errorf("library: checkpointing container %q: %w", c.cont.ID, err)
	}
	return &CheckpointResult{CompatKey: compat.HostKey(c.rt.conf.Platform, opts.Driver)}, nil
}

// Restore loads a checkpoint image into this container and starts it. The
// container must be in the Created state — which is what Runtime.Restore
// guarantees for the handles it returns; embedders that Create a container
// themselves and only later learn where the image lives call this directly.
// opts semantics are those of Runtime.Restore except that ID is taken from
// this container and no existing-container adoption happens (this IS the
// existing container).
//
// On failure, the caller is responsible for destroying the container
// (call Destroy) — the library does not auto-destroy here, only
// Runtime.Restore (the fresh-create path) cleans up on failure.
func (c *Container) Restore(opts RestoreOptions) error {
	if len(opts.GoferIOFiles) != 0 || opts.EgressFile != nil || len(opts.PassFiles) != 0 || opts.ExecFile != nil {
		return fmt.Errorf("library: gofer, egress, pass-file and executable donations require restoring to a fresh container ID")
	}
	if err := c.restore(opts); err != nil {
		return err
	}
	if opts.IngressFile != nil {
		opts.IngressFile.Close()
	}
	return nil
}

// restore borrows options and files. The entry point commits ownership only
// after its entire operation succeeds, including fresh container creation.
func (c *Container) restore(opts RestoreOptions) error {
	if opts.SplitFSRestore {
		return fmt.Errorf("library: split filesystem restore is no longer supported")
	}
	if opts.ImagePath == "" {
		return fmt.Errorf("library: restoring container %q: ImagePath is required", c.cont.ID)
	}
	if opts.ExpectedCompatKey != "" {
		if err := c.rt.checkCompatKey(opts.ExpectedCompatKey, opts.Driver); err != nil {
			return err
		}
	}
	if err := c.cont.RestoreWithOptions(c.rt.conf, container.RestoreOptions{ImagePath: opts.ImagePath, Direct: opts.Direct, Background: opts.Background, IngressFile: opts.IngressFile}); err != nil {
		return fmt.Errorf("library: restoring container %q from %q: %w", c.cont.ID, opts.ImagePath, err)
	}
	return nil
}

// Wait blocks until the container's init process exits and returns its wait
// status. It is the `runsc wait`/attached-run semantics; use it from one
// goroutine at a time.
func (c *Container) Wait() (unix.WaitStatus, error) {
	ws, err := c.cont.Wait()
	if err != nil {
		return ws, fmt.Errorf("library: waiting container %q: %w", c.cont.ID, err)
	}
	return ws, nil
}

// WaitPID is Wait for one process (by guest PID) inside the container.
func (c *Container) WaitPID(pid int32) (unix.WaitStatus, error) {
	ws, err := c.cont.WaitPID(pid)
	if err != nil {
		return ws, fmt.Errorf("library: waiting pid %d in container %q: %w", pid, c.cont.ID, err)
	}
	return ws, nil
}

// Signal sends sig to the container (`runsc kill`). With all=true every
// process in the container is signaled, otherwise only the init process.
// This is the call that is safe to make while another goroutine blocks in
// Wait.
func (c *Container) Signal(sig unix.Signal, all bool) error {
	if err := c.cont.SignalContainer(sig, all); err != nil {
		return fmt.Errorf("library: signaling container %q: %w", c.cont.ID, err)
	}
	return nil
}

// SignalProcess sends sig to a single process (by guest PID) in the
// container.
func (c *Container) SignalProcess(sig unix.Signal, pid int32) error {
	if err := c.cont.SignalProcess(sig, pid); err != nil {
		return fmt.Errorf("library: signaling pid %d in container %q: %w", pid, c.cont.ID, err)
	}
	return nil
}

// SignalProcessGroup sends sig to a process group (by guest PGID) in the
// container.
func (c *Container) SignalProcessGroup(sig unix.Signal, pgid int32) error {
	if err := c.cont.SignalProcessGroup(sig, pgid); err != nil {
		return fmt.Errorf("library: signaling pgid %d in container %q: %w", pgid, c.cont.ID, err)
	}
	return nil
}

// State returns the OCI runtime state of the container (status, PIDs,
// bundle, annotations). It is a point-in-time snapshot, not a subscription;
// see Event for usage counters.
func (c *Container) State() specs.State { return c.cont.State() }

// Execute runs a new process inside the container and returns its (guest)
// PID. args follow control.ExecArgs (the `runsc exec` surface); FDs set
// there are donated into the sandbox and consumed on success.
func (c *Container) Execute(args *control.ExecArgs) (int32, error) {
	if args == nil {
		return 0, fmt.Errorf("library: Execute requires arguments")
	}
	var donations fileDonations
	defer donations.close()
	copyArgs := *args
	var err error
	if copyArgs.Files, err = donations.copyFiles(args.Files); err != nil {
		return 0, fmt.Errorf("library: acquiring exec files: %w", err)
	}
	pid, err := c.cont.Execute(c.rt.conf, &copyArgs)
	if err != nil {
		return pid, fmt.Errorf("library: executing in container %q: %w", c.cont.ID, err)
	}
	donations.commit()
	return pid, nil
}

// Event returns a usage snapshot of the container (memory, CPU time, PIDs):
// one `runsc events --stats` payload.
func (c *Container) Event() (*boot.EventOut, error) {
	ev, err := c.cont.Event()
	if err != nil {
		return nil, fmt.Errorf("library: reading events of container %q: %w", c.cont.ID, err)
	}
	return ev, nil
}

// PortForward starts forwarding a container port over the UDS (or local
// port) carried in opts.FilePayload to the `runsc portforward` surface. The
// donated files are consumed on success and retained by the caller on error.
func (c *Container) PortForward(opts *boot.PortForwardOpts) error {
	if opts == nil {
		return fmt.Errorf("library: PortForward requires options")
	}
	var donations fileDonations
	defer donations.close()
	copyOpts := *opts
	var err error
	if copyOpts.Files, err = donations.copyFiles(opts.Files); err != nil {
		return fmt.Errorf("library: acquiring port-forward files: %w", err)
	}
	if err := c.cont.PortForward(&copyOpts); err != nil {
		return fmt.Errorf("library: port-forwarding container %q: %w", c.cont.ID, err)
	}
	donations.commit()
	return nil
}

// Pause suspends the container and its kernel (`runsc pause`).
func (c *Container) Pause() error {
	if err := c.cont.Pause(); err != nil {
		return fmt.Errorf("library: pausing container %q: %w", c.cont.ID, err)
	}
	return nil
}

// Resume resumes a paused container (`runsc resume`).
func (c *Container) Resume() error {
	if err := c.cont.Resume(); err != nil {
		return fmt.Errorf("library: resuming container %q: %w", c.cont.ID, err)
	}
	return nil
}

// Destroy tears the container down: sandbox and gofer processes are killed,
// state files under the runtime Root are removed. It is idempotent per
// handle and mandatory before dropping a handle (it is the `runsc delete -f`
// path without the ID lookup).
func (c *Container) Destroy() error {
	if err := c.cont.Destroy(); err != nil {
		return fmt.Errorf("library: destroying container %q: %w", c.cont.ID, err)
	}
	return nil
}
