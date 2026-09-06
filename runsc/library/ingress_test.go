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
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/boot/ingress"
	"gvisor.dev/gvisor/runsc/library"
	"gvisor.dev/gvisor/runsc/specutils"
)

func ingressSocket(t *testing.T) (net.Conn, *os.File) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := os.NewFile(uintptr(pair[0]), "ingress-host")
	b := os.NewFile(uintptr(pair[1]), "ingress-donation")
	host, err := net.FileConn(a)
	a.Close()
	if err != nil {
		b.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close(); b.Close() })
	return host, b
}

func ingressService(t *testing.T, ipv6 bool) (*specs.Spec, string, *os.File, netip.Addr) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("ingress integration tests require python3")
	}
	dir, err := os.MkdirTemp(testutil.TmpDir(), "ingress-service")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	portFile := createWriteableOutputFile(t, filepath.Join(dir, "port"))
	t.Cleanup(func() { portFile.Close() })
	address, family := "127.0.0.1", "AF_INET"
	if ipv6 {
		address, family = "::1", "AF_INET6"
	}
	// The daemon does not use stdin. Closing it also keeps the CLI FD-zero
	// test from checkpointing a second copy as a guest Unix stdio socket.
	script := fmt.Sprintf(`import os, socket
os.close(0)
s = socket.socket(socket.%s, socket.SOCK_STREAM)
s.bind((%q, 0))
s.listen(8)
with open(%q, "w") as f:
    f.write(str(s.getsockname()[1]))
while True:
    c, _ = s.accept()
    with c:
        while True:
            data = c.recv(65536)
            if not data:
                break
            c.sendall(data)
        c.shutdown(socket.SHUT_WR)
`, family, address, portFile.Name())
	spec := testutil.NewSpecWithArgs("python3", "-c", script)
	spec.Annotations = map[string]string{"io.kubernetes.cri.container-name": "ingress-service"}
	bundle, cleanupBundle, err := testutil.SetupBundleDir(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanupBundle)
	return spec, bundle, portFile, netip.MustParseAddr(address)
}

func ingressPort(t *testing.T, f *os.File) uint16 {
	t.Helper()
	waitForFileNotEmpty(t, f)
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 16)
	if err != nil || n == 0 {
		t.Fatalf("invalid listener port %q: %v", b, err)
	}
	return uint16(n)
}

// ingressRoundTrip verifies payload delivery and both directional EOFs. Waiting
// for the final CLOSE also ensures no carried connection is live at checkpoint.
func ingressRoundTrip(t *testing.T, host net.Conn, addr netip.Addr, port uint16) {
	t.Helper()
	if err := host.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer host.SetDeadline(time.Time{})
	write := func(b []byte) {
		t.Helper()
		if _, err := host.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	read := func(wantID uint32) (byte, []byte) {
		t.Helper()
		var prefix [4]byte
		if _, err := io.ReadFull(host, prefix[:]); err != nil {
			t.Fatal(err)
		}
		n := binary.BigEndian.Uint32(prefix[:])
		if n < ingress.MinFrameBody || n > ingress.MaxFrameBody {
			t.Fatalf("invalid frame length %d", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(host, body); err != nil {
			t.Fatal(err)
		}
		if body[0] != ingress.Version || binary.BigEndian.Uint32(body[2:6]) != wantID {
			t.Fatalf("unexpected frame %x", body)
		}
		return body[1], body[6:]
	}
	// A target that has no listener must fail only this connection. This
	// exercises the real netstack dial's error path before a successful dial.
	write(ingress.MarshalOpen(2, addr.As16(), 0))
	kind, payload := read(2)
	if kind != ingress.KindDialResult || !bytes.Equal(payload, []byte{ingress.DialRefused}) {
		t.Fatalf("refused dial: kind=%d payload=%x", kind, payload)
	}
	if kind, _ := read(2); kind != ingress.KindClose {
		t.Fatalf("refused dial missing CLOSE: %d", kind)
	}
	write(ingress.MarshalOpen(1, addr.As16(), port))
	kind, payload = read(1)
	if kind != ingress.KindDialResult || !bytes.Equal(payload, []byte{ingress.DialOK}) {
		t.Fatalf("dial failed: kind=%d payload=%x", kind, payload)
	}
	want := bytes.Repeat([]byte("ingress survives checkpoint and restore\n"), 4096)
	for remaining := want; len(remaining) > 0; {
		n := min(len(remaining), ingress.MaxDataChunk)
		write(ingress.MarshalData(1, remaining[:n]))
		remaining = remaining[n:]
	}
	write(ingress.MarshalCloseWrite(1))
	var got []byte
	halfClosed := false
	for {
		kind, payload = read(1)
		switch kind {
		case ingress.KindData:
			if halfClosed {
				t.Fatal("DATA after guest CLOSE_WRITE")
			}
			got = append(got, payload...)
		case ingress.KindCloseWrite:
			halfClosed = true
		case ingress.KindClose:
			if !halfClosed || !bytes.Equal(got, want) {
				t.Fatalf("round trip: half-close=%v bytes=%d, want %d", halfClosed, len(got), len(want))
			}
			return
		default:
			t.Fatalf("unexpected frame kind %d", kind)
		}
	}
}

func TestIngressLibraryCheckpointRestore(t *testing.T) {
	if !testutil.IsCheckpointSupported() {
		t.Skip("checkpoint not supported")
	}
	for _, ipv6 := range []bool{false, true} {
		t.Run(fmt.Sprintf("ipv6=%v", ipv6), func(t *testing.T) {
			rt, root := newTestRuntime(t)
			defer os.RemoveAll(root)
			spec, bundle, portFile, addr := ingressService(t, ipv6)
			host, file := ingressSocket(t)
			c, err := rt.Create(library.CreateOptions{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundle, IngressFile: file})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Destroy()
			if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("Create did not consume ingress file: %v", err)
			}
			if err := c.Start(); err != nil {
				t.Fatal(err)
			}
			port := ingressPort(t, portFile)
			ingressRoundTrip(t, host, addr, port)
			// Reusing the ID after CLOSE must not receive frames from the old dial.
			ingressRoundTrip(t, host, addr, port)
			host.Close()
			imageDir := filepath.Join(filepath.Dir(portFile.Name()), "image")
			if _, err := c.Checkpoint(library.CheckpointOptions{ImagePath: imageDir, Compression: statefile.CompressionLevelNone}); err != nil {
				t.Fatal(err)
			}
			if err := c.Destroy(); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"fresh", "replace", "keep-create"} {
				t.Run(mode, func(t *testing.T) {
					id := testutil.RandomContainerID()
					var oldHost net.Conn
					if mode != "fresh" {
						var oldFile *os.File
						oldHost, oldFile = ingressSocket(t)
						old, err := rt.Create(library.CreateOptions{ID: id, Spec: spec, BundleDir: bundle, IngressFile: oldFile})
						if err != nil {
							t.Fatal(err)
						}
						defer old.Destroy()
					}
					newHost, newFile := ingressSocket(t)
					if mode == "keep-create" {
						newHost.Close()
						newFile.Close()
						newHost, newFile = oldHost, nil
					}
					restored, err := rt.Restore(library.RestoreOptions{ID: id, Spec: spec, BundleDir: bundle, ImagePath: imageDir, IngressFile: newFile})
					if err != nil {
						t.Fatal(err)
					}
					defer restored.Destroy()
					if newFile != nil {
						if _, err := newFile.Stat(); !errors.Is(err, os.ErrClosed) {
							t.Fatalf("Restore did not consume ingress file: %v", err)
						}
					}
					if oldHost != nil && mode != "keep-create" {
						oldHost.SetReadDeadline(time.Now().Add(5 * time.Second))
						var b [1]byte
						if _, err := oldHost.Read(b[:]); !errors.Is(err, io.EOF) {
							t.Fatalf("superseded relay remains open: %v", err)
						}
					}
					ingressRoundTrip(t, newHost, addr, port)
				})
			}
		})
	}
}

func TestIngressCLI(t *testing.T) {
	if !testutil.IsCheckpointSupported() {
		t.Skip("checkpoint not supported")
	}
	for _, command := range []string{"create", "run"} {
		t.Run(command, func(t *testing.T) {
			rt, root := newTestRuntime(t)
			defer os.RemoveAll(root)
			spec, bundle, portFile, addr := ingressService(t, false)
			host, file := ingressSocket(t)
			id := testutil.RandomContainerID()
			args := append(rt.Config().ToFlags(), command, "--bundle", bundle, "--ingress-fd=0")
			if command == "run" {
				args = append(args, "--detach")
			}
			cmd := exec.Command(specutils.ExePath, append(args, id)...)
			cmd.Stdin = file // Explicit FD zero must be donated, not treated as absent.
			runIngressCLI(t, cmd)
			file.Close()
			c, err := rt.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Destroy()
			if command == "create" {
				if err := c.Start(); err != nil {
					t.Fatal(err)
				}
			}
			port := ingressPort(t, portFile)
			ingressRoundTrip(t, host, addr, port)
			host.Close()
			imageDir := filepath.Join(filepath.Dir(portFile.Name()), "image")
			if _, err := c.Checkpoint(library.CheckpointOptions{ImagePath: imageDir, Compression: statefile.CompressionLevelNone}); err != nil {
				t.Fatal(err)
			}
			if err := c.Destroy(); err != nil {
				t.Fatal(err)
			}
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("restore-existing=%v", existing), func(t *testing.T) {
					id := testutil.RandomContainerID()
					if existing {
						created, err := rt.Create(library.CreateOptions{ID: id, Spec: spec, BundleDir: bundle})
						if err != nil {
							t.Fatal(err)
						}
						defer created.Destroy()
					}
					newHost, newFile := ingressSocket(t)
					args := append(rt.Config().ToFlags(), "restore", "--detach", "--bundle", bundle, "--image-path", imageDir, "--ingress-fd=3", id)
					cmd := exec.Command(specutils.ExePath, args...)
					cmd.ExtraFiles = []*os.File{newFile}
					runIngressCLI(t, cmd)
					newFile.Close()
					restored, err := rt.Load(id)
					if err != nil {
						t.Fatal(err)
					}
					defer restored.Destroy()
					ingressRoundTrip(t, newHost, addr, port)
				})
			}
		})
	}
}

func TestIngressDonationFailureRetainsCallerFile(t *testing.T) {
	rt, root := newTestRuntime(t)
	defer os.RemoveAll(root)
	spec := testutil.NewSpecWithArgs("true")
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprintf("restore=%v", restore), func(t *testing.T) {
			host, file := ingressSocket(t)
			var err error
			if restore {
				_, err = rt.Restore(library.RestoreOptions{ID: testutil.RandomContainerID(), Spec: spec, ImagePath: filepath.Join(root, "missing-image"), IngressFile: file})
			} else {
				_, err = rt.Create(library.CreateOptions{ID: "invalid/id", Spec: spec, IngressFile: file})
			}
			if err == nil {
				t.Fatal("expected failure")
			}
			if _, err := file.Stat(); err != nil {
				t.Fatalf("failed operation consumed caller file: %v", err)
			}
			file.Close()
			host.SetReadDeadline(time.Now().Add(5 * time.Second))
			var b [1]byte
			if _, err := host.Read(b[:]); !errors.Is(err, io.EOF) {
				t.Fatalf("failed operation leaked a duplicate: %v", err)
			}
		})
	}
}

// Detached sandboxes inherit stdout/stderr. A file avoids waiting for pipe EOF
// from those descendants after the CLI command itself has exited.
func runIngressCLI(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	f, err := os.CreateTemp(testutil.TmpDir(), "ingress-cli-log")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Run(); err != nil {
		out, _ := os.ReadFile(f.Name())
		t.Fatalf("%v: %v\n%s", cmd.Args, err, out)
	}
}
