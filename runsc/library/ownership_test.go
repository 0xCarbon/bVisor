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

package library_test

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/library"
)

func TestLibraryDonationOwnership(t *testing.T) {
	rt, root := newTestRuntime(t)
	defer os.RemoveAll(root)
	for _, kind := range []string{"pass", "exec", "egress"} {
		for _, operation := range []string{"create", "late-create-failure", "restore-failure"} {
			t.Run(kind+"/"+operation, func(t *testing.T) {
				var file, peer *os.File
				var err error
				switch kind {
				case "pass":
					file, peer, err = os.Pipe()
				case "exec":
					file, err = os.Open("/bin/true")
				case "egress":
					var fds [2]int
					fds, err = unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
					if err == nil {
						file = os.NewFile(uintptr(fds[0]), "egress")
						peer = os.NewFile(uintptr(fds[1]), "egress-peer")
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if peer != nil {
					defer peer.Close()
				}
				spec := testutil.NewSpecWithArgs("true")
				bundle, cleanup, err := testutil.SetupBundleDir(spec)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				opts := library.CreateOptions{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundle}
				switch kind {
				case "pass":
					opts.PassFiles = map[int]*os.File{3: file}
				case "exec":
					opts.ExecFile = file
				case "egress":
					opts.EgressFile = file
				}
				var c *library.Container
				if operation == "restore-failure" {
					c, err = rt.Restore(library.RestoreOptions{
						ID: opts.ID, Spec: spec, BundleDir: bundle,
						ImagePath: filepath.Join(root, "missing-image"),
						PassFiles: opts.PassFiles, ExecFile: opts.ExecFile, EgressFile: opts.EgressFile,
					})
				} else {
					if operation == "late-create-failure" {
						// PID files are written only after the sandbox starts.
						opts.PIDFile = filepath.Join(root, "missing-parent", "pid")
					}
					c, err = rt.Create(opts)
				}
				if c != nil {
					defer c.Destroy()
				}
				if operation == "create" {
					if err != nil {
						t.Fatal(err)
					}
					if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
						t.Fatalf("successful create retained the donation: %v", err)
					}
				} else {
					if err == nil {
						t.Fatal("expected operation to fail")
					}
					want := "error writing PID file"
					if operation == "restore-failure" {
						want = "missing-image"
					}
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("operation failed before the intended phase: %v", err)
					}
					if _, err := file.Stat(); err != nil {
						t.Fatalf("failed operation consumed the donation: %v", err)
					}
				}
			})
		}
	}
}

// Exercise the successful RPC and remote-error paths, including the lifetime of
// the guest's copy after the library consumes the caller's file.
func TestLibraryRPCDonationOwnership(t *testing.T) {
	base, root := newTestRuntime(t)
	defer os.RemoveAll(root)
	rt, err := library.New(library.Options{MutateConfig: func(c *config.Config) error {
		*c = *base.Config()
		c.Network = config.NetworkHost
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	spec := testutil.NewSpecWithArgs("sleep", "1000")
	bundle, cleanup, err := testutil.SetupBundleDir(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	c, err := rt.Create(library.CreateOptions{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundle})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Destroy()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}

	t.Run("exec", func(t *testing.T) {
		file, err := os.CreateTemp(root, "exec-output")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		args := &control.ExecArgs{
			Filename:    "/missing-executable",
			Argv:        []string{"/missing-executable"},
			ContainerID: "caller-value",
			FilePayload: control.NewFilePayload(map[int]*os.File{3: file}, nil),
		}
		if _, err := c.Execute(args); err == nil {
			t.Fatal("executed a missing executable")
		}
		if _, err := file.Stat(); err != nil {
			t.Fatalf("failed exec consumed its donation: %v", err)
		}
		args.Filename = "/bin/sh"
		args.Argv = []string{"/bin/sh", "-c", "printf 'exec donation' >&3"}
		pid, err := c.Execute(args)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("successful exec retained its donation: %v", err)
		}
		if args.ContainerID != "caller-value" || args.Files[0] != file {
			t.Fatal("exec modified the caller's arguments")
		}
		status, err := c.WaitPID(pid)
		if err != nil || status.ExitStatus() != 0 {
			t.Fatalf("exec wait: status %v, error %v", status, err)
		}
		if data, err := os.ReadFile(file.Name()); err != nil || string(data) != "exec donation" {
			t.Fatalf("guest output = %q, error %v", data, err)
		}
	})

	t.Run("port-forward", func(t *testing.T) {
		listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		file := os.NewFile(uintptr(fds[0]), "forward")
		defer file.Close()
		peer := os.NewFile(uintptr(fds[1]), "probe")
		defer peer.Close()
		probe, err := net.FileConn(peer)
		if err != nil {
			t.Fatal(err)
		}
		defer probe.Close()
		if err := probe.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		opts := &boot.PortForwardOpts{
			Port:        uint16(listener.Addr().(*net.TCPAddr).Port),
			ContainerID: "caller-value",
			FilePayload: urpc.FilePayload{Files: []*os.File{file, file}},
		}
		if err := c.PortForward(opts); err == nil || !strings.Contains(err.Error(), "stream FD is required") {
			t.Fatalf("expected remote payload validation error: %v", err)
		}
		if _, err := file.Stat(); err != nil {
			t.Fatalf("failed port-forward consumed its donation: %v", err)
		}
		opts.Files = opts.Files[:1]
		if err := c.PortForward(opts); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("successful port-forward retained its donation: %v", err)
		}
		if opts.ContainerID != "caller-value" || opts.Files[0] != file {
			t.Fatal("port-forward modified the caller's options")
		}
		conn, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		for _, direction := range []struct{ from, to net.Conn }{{probe, conn}, {conn, probe}} {
			if _, err := direction.from.Write([]byte("probe")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len("probe"))
			if _, err := io.ReadFull(direction.to, buf); err != nil || string(buf) != "probe" {
				t.Fatalf("forwarded data = %q, error %v", buf, err)
			}
		}
	})
}
