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
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/library"
)

func TestRuntimeConfigurationSnapshots(t *testing.T) {
	root := t.TempDir()
	var supplied *config.Config
	rt, err := library.New(library.Options{Root: root, MutateConfig: func(c *config.Config) error {
		supplied = c
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	supplied.RootDir = "changed-after-New"
	if got := rt.Config().RootDir; got != root {
		t.Errorf("MutateConfig callback retained the runtime's configuration: %q", got)
	}
	view := rt.Config()
	view.RootDir = "changed-through-Config"
	if got := rt.Config().RootDir; got != root {
		t.Errorf("Config exposed mutable runtime state: %q", got)
	}
}

func TestCreateValidatesSpecBeforeFilesystemAccess(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, nil, 0600); err != nil {
		t.Fatal(err)
	}
	rt, err := library.New(library.Options{Root: filepath.Join(parent, "state")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		spec specs.Spec
		want string
	}{
		{"process", specs.Spec{Root: &specs.Root{Path: "/"}}, "Spec.Process must be defined"},
		{"args", specs.Spec{Process: &specs.Process{}, Root: &specs.Root{Path: "/"}}, "Spec.Process.Arg must be defined"},
		{"root", specs.Spec{Process: &specs.Process{Args: []string{"true"}}}, "Spec.Root must be defined"},
		{"root-path", specs.Spec{Process: &specs.Process{Args: []string{"true"}}, Root: &specs.Root{}}, "Spec.Root.Path must be defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rt.Create(library.CreateOptions{ID: "valid-id", Spec: &tc.spec})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Create error = %v, want spec validation before filesystem access: %s", err, tc.want)
			}
		})
	}
}

func TestCreateAppliesOCIConfigurationAnnotations(t *testing.T) {
	rt, err := library.New(library.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	spec := &specs.Spec{
		Process:     &specs.Process{Args: []string{"true"}},
		Root:        &specs.Root{Path: "/"},
		Annotations: map[string]string{"dev.gvisor.flag.network": "host"},
	}
	// An invalid ID stops before privileged work if the annotation is ignored.
	// The CLI disallows this override unless explicitly enabled by its caller.
	_, err = rt.Create(library.CreateOptions{ID: "invalid/id", Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "flag override disabled") {
		t.Fatalf("Create ignored the annotation's override policy: %v", err)
	}
}

func TestCreateRetainsCallerSpecOnFailure(t *testing.T) {
	root := t.TempDir()
	id := "stored"
	saved := &container.Container{ID: id, Status: container.Stopped,
		Saver: container.StateFile{RootDir: root, ID: container.FullID{SandboxID: id, ContainerID: id}}}
	if err := saved.Saver.LockForNew(); err != nil {
		t.Fatal(err)
	}
	err := saved.Saver.SaveLocked(saved)
	saved.Saver.UnlockOrDie()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := library.New(library.Options{Root: root, MutateConfig: func(c *config.Config) error {
		c.DirectFS = true
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	spec := &specs.Spec{Process: &specs.Process{Args: []string{"true"}}, Root: &specs.Root{Path: "/"}}
	// DirectFS adds a user namespace before the state-file collision is found.
	// No sandbox is launched, so this regression can run unprivileged.
	_, err = rt.Create(library.CreateOptions{ID: id, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "cannot lock container metadata file") {
		t.Fatalf("did not reach the expected state-file collision: %v", err)
	}
	if spec.Linux != nil {
		t.Errorf("failed Create mutated the caller's spec: %+v", spec.Linux)
	}
}
