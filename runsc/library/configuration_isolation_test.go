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
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/library"
	"gvisor.dev/gvisor/runsc/sandbox"
)

func saveConfigurationContainer(t *testing.T, root, id string, annotations map[string]string) {
	t.Helper()
	c := &container.Container{
		ID: id, Status: container.Stopped,
		Spec: &specs.Spec{
			Process: &specs.Process{Args: []string{"true"}},
			Root:    &specs.Root{Path: "/"}, Annotations: annotations,
		},
		Sandbox: &sandbox.Sandbox{ID: id},
		Saver:   container.StateFile{RootDir: root, ID: container.FullID{SandboxID: id, ContainerID: id}},
	}
	if err := c.Saver.LockForNew(); err != nil {
		t.Fatal(err)
	}
	err := c.Saver.SaveLocked(c)
	c.Saver.UnlockOrDie()
	if err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentContainerConfigurationIsolation(t *testing.T) {
	root := t.TempDir()
	rt, err := library.New(library.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		id, debug := fmt.Sprintf("stored-%d", i), i%2 == 0
		saveConfigurationContainer(t, root, id, map[string]string{"dev.gvisor.flag.debug": fmt.Sprint(debug)})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				c, err := rt.Load(id)
				if err != nil {
					t.Error(err)
					return
				}
				if got := c.Config().Debug; got != debug {
					t.Errorf("%s debug = %v, want %v", id, got, debug)
				}
				view := c.Spec()
				view.Root.Path = "/changed"
				view.Process.Args[0] = "changed"
				view.Annotations["dev.gvisor.flag.debug"] = "changed"
				if got := c.Spec(); got.Root.Path != "/" || got.Process.Args[0] != "true" || got.Annotations["dev.gvisor.flag.debug"] != fmt.Sprint(debug) {
					t.Errorf("Spec exposed shared state: %+v", got)
				}
			}
		}()
	}
	wg.Wait()
	if rt.Config().Debug {
		t.Error("a container changed the runtime's configuration")
	}
}

func TestConfigurationBundleOverridesRuntimeValues(t *testing.T) {
	root := t.TempDir()
	rt, err := library.New(library.Options{Root: root, Platform: "kvm", MutateConfig: func(c *config.Config) error {
		c.DirectFS = false
		return c.Overlay2.Set("none")
	}})
	if err != nil {
		t.Fatal(err)
	}
	saveConfigurationContainer(t, root, "bundled", map[string]string{"dev.gvisor.bundle.experimental-high-performance": "true"})
	c, err := rt.Load("bundled")
	if err != nil {
		t.Fatal(err)
	}
	got := c.Config()
	if got.Platform != "systrap" || !got.DirectFS || got.Overlay2.String() != "root:self" {
		t.Fatalf("bundle did not override the runtime's nondefault values: %+v", got)
	}
	if rt.Config().Platform != "kvm" || rt.Config().DirectFS || rt.Config().Overlay2.String() != "none" {
		t.Fatal("bundle modified the base runtime")
	}
}

func TestRestoreCompatibilityUsesAnnotatedPlatform(t *testing.T) {
	rt, err := library.New(library.Options{Root: t.TempDir(), Platform: "systrap", MutateConfig: func(c *config.Config) error {
		c.AllowFlagOverride = true
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, imagePlatform := range []string{"systrap", "kvm"} {
		t.Run(imagePlatform, func(t *testing.T) {
			key := rt.CompatKey("")
			key.Platform = imagePlatform
			spec := &specs.Spec{
				Process: &specs.Process{Args: []string{"true"}}, Root: &specs.Root{Path: "/"},
				Annotations: map[string]string{"dev.gvisor.flag.platform": "kvm"},
			}
			_, err := rt.Restore(library.RestoreOptions{
				ID: "restored", Spec: spec, ImagePath: "/missing-image", ExpectedCompatKey: key.String(),
				GoferIOFiles: []*os.File{nil}, // Stop before creating a sandbox.
			})
			if imagePlatform == "systrap" {
				if !library.IsIncompatibleKey(err) {
					t.Fatalf("accepted the base runtime's platform instead of the annotated platform: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "file 0 is nil") {
				t.Fatalf("compatible annotated platform did not reach file acquisition: %v", err)
			}
		})
	}
}
