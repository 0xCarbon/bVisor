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

// Package library_test is the reference embedder for runsc/library: every
// operation below drives containers through the library API the way oca
// will — no runsc binary is exec'ed by the TEST itself, no argv is built or
// parsed anywhere, configuration comes from library.Options, errors are
// matched with errors.As/Is* helpers, and checkpoint images carry typed
// compatibility keys.
package library_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/compat"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/library"
	"gvisor.dev/gvisor/runsc/specutils"
)

var libraryPlatform = flag.String("library-platform", "", "Platform for library lifecycle tests (empty uses the runsc default).")

func TestMain(m *testing.M) {
	config.RegisterFlags(flag.CommandLine)
	log.SetLevel(log.Debug)
	if err := testutil.ConfigureExePath(); err != nil {
		panic(err.Error())
	}
	if err := specutils.MaybeRunAsRoot(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running as root: %v\n", err)
		os.Exit(123)
	}
	os.Exit(m.Run())
}

// Removed checkpoint modes must fail before accessing a container or creating
// files; preserving the option fields must not silently change their meaning.
func TestSplitFilesystemOptionsRejected(t *testing.T) {
	var rt library.Runtime
	var c library.Container
	dir := t.TempDir()
	_, checkpointErr := c.Checkpoint(library.CheckpointOptions{ImagePath: dir, SplitFSCheckpoint: true})
	_, restoreErr := rt.Restore(library.RestoreOptions{ImagePath: dir, SplitFSRestore: true})
	containerRestoreErr := c.Restore(library.RestoreOptions{ImagePath: dir, SplitFSRestore: true})
	for name, err := range map[string]error{
		"checkpoint":        checkpointErr,
		"runtime restore":   restoreErr,
		"container restore": containerRestoreErr,
	} {
		if err == nil || !strings.Contains(err.Error(), "no longer supported") {
			t.Errorf("%s error = %v, want unsupported split filesystem error", name, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("unsupported operations created files: %v", entries)
	}
}

// defaultConfig returns the runsc flag-registration defaults — the config a
// bare runsc binary runs with — WITHOUT parsing any argv: the flag set is
// never Parse()d, exactly like library.New does internally. The reference
// test uses it only to select the build's default platform and network.
func defaultConfig(t *testing.T) *config.Config {
	t.Helper()
	fs := flag.NewFlagSet("library-test-defaults", flag.ContinueOnError)
	config.RegisterFlags(fs)
	conf, err := config.NewFromFlags(fs)
	if err != nil {
		t.Fatalf("config.NewFromFlags(defaults): %v", err)
	}
	return conf
}

// newTestRuntime builds a library Runtime on a fresh root directory with the
// build's default platform, the argv-free way. The test-host knobs that
// testutil.TestConfig applies to the CLI-driven tests (run without chroot,
// no network stack, no overlay) are reached through Options.MutateConfig —
// the escape hatch an embedder uses for every flag Options does not name.
func newTestRuntime(t *testing.T) (*library.Runtime, string) {
	t.Helper()
	def := defaultConfig(t)
	if *libraryPlatform != "" {
		def.Platform = *libraryPlatform
	}
	rootDir, err := os.MkdirTemp(testutil.TmpDir(), "library-root")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	rt, err := library.New(library.Options{
		Root:     rootDir,
		Platform: def.Platform,
		Network:  config.NetworkNone.String(),
		MutateConfig: func(c *config.Config) error {
			c.TestOnlyAllowRunAsCurrentUserWithoutChroot = true
			c.Overlay2.Set("none")
			c.WatchdogAction = "panic"
			return nil
		},
	})
	if err != nil {
		os.RemoveAll(rootDir)
		t.Fatalf("library.New: %v", err)
	}
	return rt, rootDir
}

// TestRuntimeConfigArgvFree checks the argv-free configuration surface:
// Options map onto the same *config.Config the CLI flag set produces, the
// MutateConfig escape hatch runs before validation, and CompatKey round-trips
// through its serialized form.
func TestRuntimeConfigArgvFree(t *testing.T) {
	rt, rootDir := newTestRuntime(t)
	defer os.RemoveAll(rootDir)

	def := defaultConfig(t)
	conf := rt.Config()
	if conf.RootDir != rootDir {
		t.Errorf("Config().RootDir = %q, want %q", conf.RootDir, rootDir)
	}
	wantPlatform := def.Platform
	if *libraryPlatform != "" {
		wantPlatform = *libraryPlatform
	}
	if conf.Platform != wantPlatform {
		t.Errorf("Config().Platform = %q, want %q", conf.Platform, wantPlatform)
	}
	if conf.Network != config.NetworkNone {
		t.Errorf("Config().Network = %v, want %v (Options.Network)", conf.Network, config.NetworkNone)
	}
	if !conf.TestOnlyAllowRunAsCurrentUserWithoutChroot || conf.WatchdogAction != "panic" {
		t.Errorf("Options.MutateConfig hook did not run on the runtime config")
	}

	// The escape hatch: every runsc flag not named in Options is still
	// reachable, applied before validation.
	rt2, err := library.New(library.Options{
		Root:     rootDir,
		Platform: def.Platform,
		Network:  def.Network.String(),
		MutateConfig: func(c *config.Config) error {
			c.DisableSeccomp = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("library.New(MutateConfig): %v", err)
	}
	if !rt2.Config().DisableSeccomp {
		t.Errorf("MutateConfig hook did not run (DisableSeccomp = false)")
	}

	// Compat key round-trip: String() and Parse are inverse, and the key
	// carries the identity fields embedders gate placement on.
	key := rt.CompatKey("")
	if key.Build == "" || key.Platform == "" || key.CPUFeaturesID == "" {
		t.Errorf("CompatKey(%q) missing identity fields: %+v", "", key)
	}
	parsed, err := compat.Parse(key.String())
	if err != nil {
		t.Fatalf("compat.Parse(%q): %v", key.String(), err)
	}
	if parsed != key {
		t.Errorf("compat key round-trip: parsed %+v, want %+v", parsed, key)
	}
	if !key.Compatible(key) {
		t.Errorf("compat key not compatible with itself: %+v", key)
	}
}

// TestLibraryIncompatibleKeyFailsFast verifies the typed, pre-sandbox
// compatibility gate: Restore with an ExpectedCompatKey that does not match
// this host fails with *library.IncompatibleKey before any spec is read or
// sandbox process is built.
func TestLibraryIncompatibleKeyFailsFast(t *testing.T) {
	rt, rootDir := newTestRuntime(t)
	defer os.RemoveAll(rootDir)

	host := rt.CompatKey("")
	foreign := host
	foreign.Build = "some-other-runsc-build"
	// The image path does not exist and no bundle is provided: a restore
	// that got past the compat gate would fail on the spec/image instead,
	// so hitting IncompatibleKey proves the gate ran first.
	_, err := rt.Restore(library.RestoreOptions{
		ID:                testutil.RandomContainerID(),
		ImagePath:         "/nonexistent/checkpoint-image",
		ExpectedCompatKey: foreign.String(),
	})
	if !library.IsIncompatibleKey(err) {
		t.Fatalf("rt.Restore(mismatched key) error = %v, want *library.IncompatibleKey", err)
	}
	var ik *library.IncompatibleKey
	if !errors.As(err, &ik) {
		t.Fatalf("errors.As(*library.IncompatibleKey) failed for %v", err)
	}
	if ik.Image != foreign || ik.Host != host {
		t.Errorf("IncompatibleKey fields: image %+v host %+v, want image %+v host %+v", ik.Image, ik.Host, foreign, host)
	}
	// Fast-fail proof: nothing was created under the runtime root.
	if ents, rerr := os.ReadDir(rootDir); rerr == nil && len(ents) != 0 {
		t.Errorf("failed restore left state behind: %v", ents)
	}

	// A malformed key is a parse error, not a compatibility verdict.
	_, err = rt.Restore(library.RestoreOptions{
		ID:                testutil.RandomContainerID(),
		ImagePath:         "/nonexistent/checkpoint-image",
		ExpectedCompatKey: "not-a-key",
	})
	if err == nil || library.IsIncompatibleKey(err) {
		t.Errorf("rt.Restore(malformed key) error = %v, want a parse error that is not IncompatibleKey", err)
	}
}

// createWriteableOutputFile creates the host file the container writes to.
func createWriteableOutputFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o666)
	if err != nil {
		t.Fatalf("error creating file %q: %v", path, err)
	}
	if err := f.Chmod(0o666); err != nil { // Allow writing after umask.
		t.Fatalf("error chmoding file %q: %v", path, err)
	}
	return f
}

// waitForFileNotEmpty polls until f has contents.
func waitForFileNotEmpty(t *testing.T, f *os.File) {
	t.Helper()
	op := func() error {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if fi.Size() == 0 {
			return fmt.Errorf("file %q is empty", f.Name())
		}
		return nil
	}
	if err := testutil.Poll(op, 30*time.Second); err != nil {
		t.Fatalf("waitForFileNotEmpty: %v", err)
	}
}

// procDead reports whether pid is gone or a zombie.
func procDead(pid int) bool {
	if err := unix.Kill(pid, 0); err != nil {
		return true
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	if i := bytes.LastIndexByte(data, ')'); i >= 0 && i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}

// TestLibraryEmbedderLifecycle is the reference embedder flow, exactly as
// the package doc sketches it: create (with FD donation), start, checkpoint
// (compat-keyed image), destroy, restore into a NEW container ID re-donating
// DIFFERENT files under the saved guest descriptors, exec in the restored
// container, destroy. It doubles as the pass-FD re-donation proof through
// the library surface: guest fd 3 is a host pipe at create time and a
// different host pipe at restore time, and the restored task must consume
// the second one.
func TestLibraryEmbedderLifecycle(t *testing.T) {
	if !testutil.IsCheckpointSupported() {
		t.Skip("Checkpoint not supported")
	}
	rt, rootDir := newTestRuntime(t)
	defer os.RemoveAll(rootDir)

	dir, err := os.MkdirTemp(testutil.TmpDir(), "library-lifecycle")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)

	outputPath := filepath.Join(dir, "output")
	outputFile := createWriteableOutputFile(t, outputPath)
	defer outputFile.Close()

	// The container blocks reading guest fd 3 and tees it to the output
	// file; the container-name annotation is the re-donation key's name
	// component and must be identical across checkpoint and restore.
	const containerName = "w06-library-embedder"
	spec := testutil.NewSpecWithArgs("bash", "-c", fmt.Sprintf("cat <&3 > %q", outputPath))
	spec.Annotations = map[string]string{
		"io.kubernetes.cri.container-name": containerName,
	}
	bundleDir, cleanupBundle, err := testutil.SetupBundleDir(spec)
	if err != nil {
		t.Fatalf("testutil.SetupBundleDir: %v", err)
	}
	defer cleanupBundle()

	// Guest fd 3 is the FIRST pipe's read end; it stays empty so the task
	// blocks in read and survives until the checkpoint. After a successful
	// Create the file is consumed by runsc (donated and closed host-side);
	// the embedder must simply drop it.
	pipeARead, pipeAWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pipeAWrite.Close()

	c, err := rt.Create(library.CreateOptions{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
		PassFiles: map[int]*os.File{3: pipeARead},
	})
	if err != nil {
		t.Fatalf("rt.Create: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("c.Start: %v", err)
	}
	// Let the task reach the blocking read before freezing it.
	time.Sleep(2 * time.Second)

	imageDir := filepath.Join(dir, "image")
	res, err := c.Checkpoint(library.CheckpointOptions{
		ImagePath:   imageDir,
		Compression: statefile.CompressionLevelNone,
	})
	if err != nil {
		c.Destroy()
		t.Fatalf("c.Checkpoint: %v", err)
	}
	// The image carries its compatibility class; it must be exactly this
	// host's key (Checkpoint succeeded here).
	if got, want := res.CompatKey.String(), rt.CompatKey("").String(); got != want {
		t.Errorf("CheckpointResult.CompatKey = %q, want host key %q", got, want)
	}
	if err := c.Destroy(); err != nil {
		t.Fatalf("c.Destroy: %v", err)
	}
	// The original pipe is irrelevant from here on.
	pipeAWrite.Close()

	// Restore into a NEW container ID, re-donating a DIFFERENT pipe under
	// the same guest descriptor number, and gate on the recorded key.
	pipeBRead, pipeBWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pipeBRead.Close()

	c2, err := rt.Restore(library.RestoreOptions{
		ID:                testutil.RandomContainerID(),
		ImagePath:         imageDir,
		Spec:              spec,
		BundleDir:         bundleDir,
		PassFiles:         map[int]*os.File{3: pipeBRead},
		ExpectedCompatKey: res.CompatKey.String(),
	})
	if err != nil {
		t.Fatalf("rt.Restore: %v", err)
	}
	defer c2.Destroy()
	if state := c2.State(); state.Status != specs.StateRunning {
		t.Fatalf("restored container state = %q, want %q", state.Status, specs.StateRunning)
	}

	// Exec liveness inside the restored container while its init task is
	// still blocked reading fd 3: a fresh process runs and exits 0,
	// observed through the library Execute/WaitPID pair.
	pid, err := c2.Execute(&control.ExecArgs{
		Filename: "/bin/sh",
		Argv:     []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("c2.Execute: %v", err)
	}
	ws, err := c2.WaitPID(pid)
	if err != nil {
		t.Fatalf("c2.WaitPID(%d): %v", pid, err)
	}
	if ws.ExitStatus() != 0 {
		t.Errorf("exec in restored container exited %d, want 0", ws.ExitStatus())
	}

	// The restored task must read the SECOND pipe: host resources behind
	// saved guest descriptors are re-bound to what restore donates. The
	// write also EOFs the task (init exits, sandbox winds down), so this
	// is the last observation made on the live container.
	const donated = "re-donated via library\n"
	if _, err := pipeBWrite.Write([]byte(donated)); err != nil {
		t.Fatalf("writing to donated pipe: %v", err)
	}
	pipeBWrite.Close()
	waitForFileNotEmpty(t, outputFile)
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", outputPath, err)
	}
	if string(got) != donated {
		t.Errorf("restored task output = %q, want %q (guest fd 3 must read the pipe donated at restore)", string(got), donated)
	}

	if err := c2.Destroy(); err != nil {
		t.Fatalf("c2.Destroy: %v", err)
	}
}

// TestLibraryCheckpointSandboxDeath verifies the typed checkpoint-failure
// taxonomy through the library: a sandbox killed mid-flight makes
// Container.Checkpoint fail with an error that errors.As recognizes as
// *library.SandboxDeath (the in-process class the argv driver could only
// substring-match), and no partial image is left behind.
func TestLibraryCheckpointSandboxDeath(t *testing.T) {
	if !testutil.IsCheckpointSupported() {
		t.Skip("Checkpoint not supported")
	}
	rt, rootDir := newTestRuntime(t)
	defer os.RemoveAll(rootDir)

	dir, err := os.MkdirTemp(testutil.TmpDir(), "library-sandbox-death")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)

	outputPath := filepath.Join(dir, "output")
	outputFile := createWriteableOutputFile(t, outputPath)
	defer outputFile.Close()

	script := fmt.Sprintf("i=0; while true; do echo $i >> %q; sleep 1; i=$((i+1)); done", outputPath)
	spec := testutil.NewSpecWithArgs("bash", "-c", script)
	bundleDir, cleanupBundle, err := testutil.SetupBundleDir(spec)
	if err != nil {
		t.Fatalf("testutil.SetupBundleDir: %v", err)
	}
	defer cleanupBundle()

	c, err := rt.Create(library.CreateOptions{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	})
	if err != nil {
		t.Fatalf("rt.Create: %v", err)
	}
	defer c.Destroy()
	if err := c.Start(); err != nil {
		t.Fatalf("c.Start: %v", err)
	}
	waitForFileNotEmpty(t, outputFile)

	// Kill the sandbox the way an unexpected sentry death would; wait for
	// it to be gone so the checkpoint RPC fails on a closed channel.
	sandboxPid := c.SandboxPid()
	if err := unix.Kill(sandboxPid, unix.SIGKILL); err != nil {
		t.Fatalf("unix.Kill(%d, SIGKILL): %v", sandboxPid, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !procDead(sandboxPid) {
		if time.Now().After(deadline) {
			t.Fatalf("sandbox process %d did not die after SIGKILL", sandboxPid)
		}
		time.Sleep(10 * time.Millisecond)
	}

	imageDir := filepath.Join(dir, "image")
	_, err = c.Checkpoint(library.CheckpointOptions{
		ImagePath:   imageDir,
		Compression: statefile.CompressionLevelNone,
	})
	if err == nil {
		t.Fatalf("checkpoint unexpectedly succeeded on a dead sandbox")
	}
	if !library.IsSandboxDeath(err) {
		t.Errorf("library.IsSandboxDeath(%v) = false, want true", err)
	}
	var sd *library.SandboxDeath
	if !errors.As(err, &sd) {
		t.Errorf("errors.As(*library.SandboxDeath) = false for %v", err)
	}
	if library.IsSaveRejection(err) {
		t.Errorf("library.IsSaveRejection(%v) = true, want false (death is not a save rejection)", err)
	}
	// The failed checkpoint must not leave image files behind.
	for _, name := range []string{"checkpoint.img", "pages_meta.img", "pages.img"} {
		if _, statErr := os.Stat(filepath.Join(imageDir, name)); !os.IsNotExist(statErr) {
			t.Errorf("image file %q left behind by failed checkpoint (error: %v)", name, err)
		}
	}
}
