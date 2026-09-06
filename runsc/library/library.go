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

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/specutils"
)

// Options configures a Runtime. The zero value is usable: it defaults to the
// runsc flag defaults (platform systrap, network sandbox, root
// config.DefaultRootDir) exactly as a runsc binary invoked with no flags.
type Options struct {
	// Root is the runtime root directory for container state
	// (runsc --root). Empty uses config.DefaultRootDir(). The directory is
	// created on first use.
	Root string

	// ExePath is the path to the runsc binary used to spawn the sandbox
	// and gofer processes. The default
	// "/proc/self/exe" re-executes the EMBEDDING process as the sentry —
	// embedders that are not the runsc binary itself must set this to a
	// real runsc binary path (the reference test does via the test
	// harness). Install the matching gvisor-bin/ sidecars next to that
	// binary; build //:release to produce the complete layout.
	ExePath string

	// Platform is the sentry platform: "systrap" (default), "ptrace",
	// "kvm". It participates in the restore-compatibility key.
	Platform string

	// Network is the network stack: "sandbox" (default), "host", "none".
	Network string

	// Debug enables debug logging to stderr (runsc --debug).
	Debug bool

	// DebugLog, when non-empty, is a runsc --debug-log pattern (supports
	// %TIMESTAMP% and %COMMAND% variables, trailing '/' means a
	// directory).
	DebugLog string

	// NVProxy enables GPU support (runsc --nvproxy). The per-sandbox
	// switch is the dev.gvisor.internal.nvproxy annotation; this is the
	// host-wide enablement.
	NVProxy bool

	// NVProxyAllowedDriverCapabilities, when non-empty, is the runsc
	// --nvproxy-allowed-driver-capabilities CSV.
	NVProxyAllowedDriverCapabilities string

	// MutateConfig, when non-nil, is applied to the freshly-defaulted
	// *config.Config before validation. It is the escape hatch for every
	// runsc flag this Options struct does not name; field names and
	// semantics are the runsc flag documentation.
	MutateConfig func(*config.Config) error
}

// Runtime is an argv-free runsc runtime: an immutable configuration plus the
// root directory that container state lives under. Create one per
// configuration; share it across goroutines; build Containers from it.
type Runtime struct {
	conf *config.Config
}

// New validates opts and returns a Runtime. It never touches the network,
// mounts, or privileged state; the first privileged operation happens on
// Create/Restore.
func New(opts Options) (*Runtime, error) {
	// Build the config from flag-registration defaults, the same mechanism
	// the CLI uses for an unparsed flag set (and testutil.TestConfig for
	// tests): RegisterFlags installs every default, NewFromFlags snapshots
	// them into a Config and validates it. No argv is parsed anywhere.
	fs := flag.NewFlagSet("runsc-library", flag.ContinueOnError)
	config.RegisterFlags(fs)
	conf, err := config.NewFromFlags(fs)
	if err != nil {
		return nil, fmt.Errorf("building default runsc config: %w", err)
	}

	if opts.Root != "" {
		conf.RootDir = opts.Root
	}
	// Snapshot the launcher default so this runtime never needs to mutate
	// specutils.ExePath, including during later checkpoint gofer launches.
	conf.ExecutablePath = specutils.ExePath
	if opts.ExePath != "" {
		conf.ExecutablePath = opts.ExePath
	}
	if opts.Platform != "" {
		conf.Platform = opts.Platform
	}
	if opts.Network != "" {
		if err := conf.Network.Set(opts.Network); err != nil {
			return nil, fmt.Errorf("invalid network type %q: %w", opts.Network, err)
		}
	}
	conf.Debug = opts.Debug
	if opts.DebugLog != "" {
		conf.DebugLog = opts.DebugLog
	}
	conf.NVProxy = opts.NVProxy
	if opts.NVProxyAllowedDriverCapabilities != "" {
		conf.NVProxyAllowedDriverCapabilities = opts.NVProxyAllowedDriverCapabilities
	}
	if opts.MutateConfig != nil {
		if err := opts.MutateConfig(conf); err != nil {
			return nil, fmt.Errorf("configuring runtime: %w", err)
		}
	}
	if err := conf.Validate(); err != nil {
		return nil, fmt.Errorf("invalid runtime configuration: %w", err)
	}
	return &Runtime{conf: conf}, nil
}

// Config returns the runsc configuration this runtime was built with. The
// returned config is shared: callers must not mutate it (copy first).
func (r *Runtime) Config() *config.Config {
	return r.conf
}

// CreateOptions are the arguments for Runtime.Create: the library view of
// container.Args plus the runsc create flags that matter to embedders.
type CreateOptions struct {
	// ID is the container ID (required, validated by runsc).
	ID string

	// Spec is the OCI runtime spec (required).
	Spec *specs.Spec

	// BundleDir is the bundle directory the spec was read from; stored in
	// container state and reused by Restore defaults.
	BundleDir string

	// ConsoleSocket, PIDFile and UserLog mirror the corresponding runsc
	// create flags; all optional.
	ConsoleSocket string
	PIDFile       string
	UserLog       string

	// Attached ties the sandbox lifecycle to this process: when the
	// process exits, the sandbox is torn down (runsc create/restore
	// --detach=false). Defaults to detached (library callers usually
	// outlive their sandboxes; oca passes explicit false today).
	Attached bool

	// GoferIOFiles are the sandbox-side LISAFS connections of an external
	// gofer, one per gofer-backed mount in spec order (root first); the
	// wave-02 --io-fds donation. When set, runsc spawns no gofer process
	// of its own. Consumed on success (see package doc OWNERSHIP).
	GoferIOFiles []*os.File

	// EgressFile is the AF_UNIX connection to the egress flow gate that is
	// re-donated into the sandbox netstack (wave-02 --egress-fd).
	// Consumed on success.
	EgressFile *os.File

	// IngressFile is the AF_UNIX connection to the host-ingress relay that
	// is re-donated into the sandbox netstack (Oca #541 --ingress-fd).
	// Consumed on success.
	IngressFile *os.File

	// PassFiles are host files bound to guest descriptor numbers; they are
	// re-donated (not serialized) across checkpoint/restore. Consumed on
	// success.
	PassFiles map[int]*os.File

	// ExecFile is the host executable file for the container init process.
	// Consumed on success.
	ExecFile *os.File

	// FSRestoreImagePath/FSRestoreDirect mirror the split-filesystem
	// restore flags of runsc create (optional).
	FSRestoreImagePath string
	FSRestoreDirect    bool
}

// Create creates (but does not start) a container in a new sandbox. It
// returns a handle whose lifecycle the caller owns: call Destroy when done.
func (r *Runtime) Create(opts CreateOptions) (*Container, error) {
	if opts.Spec == nil {
		return nil, fmt.Errorf("library: Create requires a Spec")
	}
	args := container.Args{
		ID:                 opts.ID,
		Spec:               opts.Spec,
		BundleDir:          opts.BundleDir,
		ConsoleSocket:      opts.ConsoleSocket,
		PIDFile:            opts.PIDFile,
		UserLog:            opts.UserLog,
		Attached:           opts.Attached,
		IOFiles:            opts.GoferIOFiles,
		EgressFile:         opts.EgressFile,
		IngressFile:        opts.IngressFile,
		PassFiles:          opts.PassFiles,
		ExecFile:           opts.ExecFile,
		FSRestoreImagePath: opts.FSRestoreImagePath,
		FSRestoreDirect:    opts.FSRestoreDirect,
	}
	var donations fileDonations
	defer donations.close()
	if err := donations.acquire(&args); err != nil {
		return nil, fmt.Errorf("library: %w", err)
	}
	c, err := container.New(r.conf, args)
	if err != nil {
		return nil, fmt.Errorf("library: creating container %q: %w", opts.ID, err)
	}
	donations.commit()
	return &Container{rt: r, cont: c}, nil
}

// Load adopts an existing container from the runtime root directory (any
// prefix of the ID that is unique, like the CLI). The container may have
// been created by this Runtime, another Runtime in the same process, or a
// runsc CLI invocation on the same root.
func (r *Runtime) Load(id string) (*Container, error) {
	c, err := container.Load(r.conf.RootDir, container.FullID{ContainerID: id}, container.LoadOpts{})
	if err != nil {
		return nil, fmt.Errorf("library: loading container %q: %w", id, err)
	}
	return &Container{rt: r, cont: c}, nil
}

// RestoreOptions are the arguments for Runtime.Restore: the library view of
// the runsc restore command.
type RestoreOptions struct {
	// ID is the new container ID (required).
	ID string

	// ImagePath is the checkpoint image directory (required).
	ImagePath string

	// Spec is the OCI spec for a fresh restored container. When nil it is
	// read from BundleDir/config.json. Existing containers use their saved
	// spec and do not need the bundle to remain available.
	Spec *specs.Spec

	// BundleDir is the bundle directory for the spec (defaults to the
	// current working directory when Spec is nil).
	BundleDir string

	// ConsoleSocket, PIDFile and UserLog mirror the runsc restore flags.
	ConsoleSocket string
	PIDFile       string
	UserLog       string

	// Attached ties the sandbox lifecycle to this process (see
	// CreateOptions.Attached). The CLI default is attached; oca restores
	// detached.
	Attached bool

	// Direct reads the checkpoint pages with O_DIRECT; Background lets
	// image loading continue after the call returns (uncompressed images
	// only). Both mirror the runsc restore flags.
	Direct     bool
	Background bool

	// SplitFSRestore is retained for source compatibility.
	// Deprecated: upstream removed split filesystem checkpoints. Setting
	// this field returns an error before creating or adopting a container.
	SplitFSRestore bool

	// GoferIOFiles, EgressFile, PassFiles and ExecFile are the restore-side
	// FD donations; an external-gofer sandbox must re-donate its gofer
	// connections here, and saved guest descriptors re-bind to the
	// PassFiles given now (see package doc). Consumed on success.
	//
	// These donations apply only to a fresh container. They are rejected
	// when adopting an existing container, whose create-time donations
	// cannot be replaced by the restore RPC.
	GoferIOFiles []*os.File
	EgressFile   *os.File
	PassFiles    map[int]*os.File
	ExecFile     *os.File

	// IngressFile replaces any create-time socket, including when ID already
	// exists. Consumed only on success; retained by the caller on failure.
	IngressFile *os.File

	// ExpectedCompatKey, when non-empty, is a serialized compat.Key (e.g.
	// the CheckpointResult.CompatKey.String() recorded when the image was
	// written). Restore verifies this host can accept the image BEFORE
	// building any sandbox, and fails with a *IncompatibleKey (typed,
	// in-process — this is the check embedders used to hand-roll from
	// `runsc --version` + cpu annotations) on mismatch.
	ExpectedCompatKey string

	// Driver is the GPU driver class to assume in the compatibility key
	// when ExpectedCompatKey is set (empty for CPU sandboxes; mirrors
	// CheckpointOptions.Driver).
	Driver string
}

// Restore creates a new container from a checkpoint image and starts it. It
// mirrors the runsc restore command: if a container with opts.ID already
// exists in the root it is reused, otherwise a fresh one is created from the
// spec; the image is then loaded and the application resumed.
func (r *Runtime) Restore(opts RestoreOptions) (*Container, error) {
	if opts.SplitFSRestore {
		return nil, fmt.Errorf("library: split filesystem restore is no longer supported")
	}
	if opts.ImagePath == "" {
		return nil, fmt.Errorf("library: Restore requires an ImagePath")
	}
	if opts.ExpectedCompatKey != "" {
		if err := r.checkCompatKey(opts.ExpectedCompatKey, opts.Driver); err != nil {
			return nil, err
		}
	}

	// Adopt an existing container if present (the CLI semantics); the
	// state file may have been left by a previous Create of this ID.
	// Note the CLI contract: the loaded container keeps the spec it was
	// created with (c.Spec), not opts.Spec. Only a fresh container needs
	// its spec read from a bundle.
	c, err := container.Load(r.conf.RootDir, container.FullID{ContainerID: opts.ID}, container.LoadOpts{})
	if err == nil {
		lc := &Container{rt: r, cont: c}
		if err := lc.Restore(opts); err != nil {
			return nil, err
		}
		return lc, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("library: loading container %q: %w", opts.ID, err)
	}

	spec := opts.Spec
	if spec == nil {
		bundleDir := opts.BundleDir
		if bundleDir == "" {
			wd, err := os.Getwd()
			if err != nil {
				return nil, fmt.Errorf("library: resolving default bundle dir: %w", err)
			}
			bundleDir = wd
		}
		s, err := specutils.ReadSpec(bundleDir, r.conf)
		if err != nil {
			return nil, fmt.Errorf("library: reading spec from bundle %q: %w", bundleDir, err)
		}
		spec = s
		opts.BundleDir = bundleDir
	}

	args := container.Args{
		ID:              opts.ID,
		Spec:            spec,
		BundleDir:       opts.BundleDir,
		ConsoleSocket:   opts.ConsoleSocket,
		PIDFile:         opts.PIDFile,
		UserLog:         opts.UserLog,
		Attached:        opts.Attached,
		IOFiles:         opts.GoferIOFiles,
		EgressFile:      opts.EgressFile,
		IngressFile:     opts.IngressFile,
		PassFiles:       opts.PassFiles,
		ExecFile:        opts.ExecFile,
		FSRestoreDirect: opts.Direct,
	}
	var donations fileDonations
	defer donations.close()
	if err := donations.acquire(&args); err != nil {
		return nil, fmt.Errorf("library: %w", err)
	}
	c, err = container.New(r.conf, args)
	if err != nil {
		return nil, fmt.Errorf("library: creating container %q for restore: %w", opts.ID, err)
	}
	lc := &Container{rt: r, cont: c}
	if err := lc.restore(opts); err != nil {
		// Mirror the CLI cleanup: destroy the partially created container.
		if derr := c.Destroy(); derr != nil {
			log.Warningf("library: destroying partially restored container %q: %v", opts.ID, derr)
		}
		return nil, err
	}
	donations.commit()
	return lc, nil
}
