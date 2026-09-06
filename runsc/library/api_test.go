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
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/library"
	"gvisor.dev/gvisor/runsc/sandbox"
	"gvisor.dev/gvisor/runsc/specutils"
)

// Invalid IDs stop before privileged work, allowing the library's acquisition
// and isolation contracts to be tested by ordinary external consumers.
func TestCreateFailureRetainsDonations(t *testing.T) {
	for _, name := range []string{"gofer", "egress", "ingress", "pass", "exec"} {
		t.Run(name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "donation")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			rt, err := library.New(library.Options{Root: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			opts := library.CreateOptions{ID: "invalid/id", Spec: &specs.Spec{}}
			switch name {
			case "gofer":
				opts.GoferIOFiles = []*os.File{f}
			case "egress":
				opts.EgressFile = f
			case "ingress":
				opts.IngressFile = f
			case "pass":
				opts.PassFiles = map[int]*os.File{10: f}
			case "exec":
				opts.ExecFile = f
			}
			if _, err := rt.Create(opts); err == nil {
				t.Fatal("Create accepted an invalid ID")
			}
			if _, err := f.Stat(); err != nil {
				t.Errorf("failed Create consumed the caller's %s donation: %v", name, err)
			}
		})
	}
}

func TestRestoreExistingContainerWithoutBundle(t *testing.T) {
	root := t.TempDir()
	id := "stored"
	saved := &container.Container{
		ID: id, Status: container.Stopped, Spec: &specs.Spec{},
		Sandbox: &sandbox.Sandbox{ID: id},
		Saver:   container.StateFile{RootDir: root, ID: container.FullID{SandboxID: id, ContainerID: id}},
	}
	if err := saved.Saver.LockForNew(); err != nil {
		t.Fatal(err)
	}
	err := saved.Saver.SaveLocked(saved)
	saved.Saver.UnlockOrDie()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := library.New(library.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.Restore(library.RestoreOptions{ID: id, ImagePath: t.TempDir(), BundleDir: "/does-not-exist"})
	// The stored container is stopped, so its lifecycle validation must run.
	// Requiring a bundle first would conceal that error and break valid restores
	// whose original bundle has been removed.
	if err == nil || strings.Contains(err.Error(), "reading spec") || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("restore did not reach the stored container: %v", err)
	}
	file, err := os.CreateTemp(t.TempDir(), "unused-donation")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	_, err = rt.Restore(library.RestoreOptions{ID: id, ImagePath: t.TempDir(), EgressFile: file})
	if err == nil || !strings.Contains(err.Error(), "fresh container ID") {
		t.Fatalf("restore accepted a donation it cannot use: %v", err)
	}
	if _, err := file.Stat(); err != nil {
		t.Fatalf("rejected donation was consumed: %v", err)
	}
}

func TestDonationAcquisitionFailureRetainsEarlierFiles(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "first")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rt, err := library.New(library.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.Create(library.CreateOptions{ID: "invalid/id", Spec: &specs.Spec{}, GoferIOFiles: []*os.File{f, nil}})
	if err == nil {
		t.Fatal("accepted a nil gofer file")
	}
	if _, err := f.Stat(); err != nil {
		t.Fatalf("partial acquisition consumed an earlier file: %v", err)
	}
}

func TestConcurrentRuntimeCreate(t *testing.T) {
	original := specutils.ExePath
	t.Cleanup(func() { specutils.ExePath = original })
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		rt, err := library.New(library.Options{Root: t.TempDir(), ExePath: fmt.Sprintf("/runtime-%d/runsc", i)})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 1000; j++ {
				if _, err := rt.Create(library.CreateOptions{ID: "invalid/id", Spec: &specs.Spec{}}); err == nil {
					t.Error("Create accepted an invalid ID")
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if specutils.ExePath != original {
		t.Errorf("concurrent runtimes changed the process executable: got %q, want %q", specutils.ExePath, original)
	}
}
