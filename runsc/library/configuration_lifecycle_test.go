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
	"os"
	"path/filepath"
	"testing"

	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/library"
)

func TestLibraryConfigurationAcrossLifecycle(t *testing.T) {
	base, root := newTestRuntime(t)
	defer os.RemoveAll(root)
	rt, err := library.New(library.Options{MutateConfig: func(c *config.Config) error {
		*c = *base.Config()
		c.AllowFlagOverride = true
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
	absoluteRoot := spec.Root.Path
	spec.Root.Path, err = filepath.Rel(bundle, absoluteRoot)
	if err != nil {
		t.Fatal(err)
	}
	relativeRoot := spec.Root.Path
	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}
	spec.Annotations["dev.gvisor.flag.network"] = "host"
	c, err := rt.Create(library.CreateOptions{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundle})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Destroy()
	if got := c.Spec().Root.Path; got != absoluteRoot {
		t.Fatalf("container root = %q, want normalized %q", got, absoluteRoot)
	}
	if spec.Root.Path != relativeRoot {
		t.Fatalf("Create rewrote the caller's relative root: %q", spec.Root.Path)
	}
	if c.Config().Network != config.NetworkHost || rt.Config().Network != config.NetworkNone {
		t.Fatal("Create did not isolate the annotated configuration")
	}
	// Start must use the host-network configuration used to create this
	// sandbox, rather than trying to configure a sandbox netstack.
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	loaded, err := rt.Load(c.ID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config().Network != config.NetworkHost {
		t.Fatal("Load lost the container's configuration annotations")
	}
	view := loaded.Spec()
	view.Root.Path = "/changed"
	if loaded.Spec().Root.Path != absoluteRoot {
		t.Fatal("Spec returned shared mutable state")
	}
}
