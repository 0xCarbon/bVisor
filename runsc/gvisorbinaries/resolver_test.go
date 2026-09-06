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

package gvisorbinaries

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/version"
)

func resolverInstallation(t *testing.T, exitCode int) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, binDirName), 0700); err != nil {
		t.Fatal(err)
	}
	for _, b := range All {
		path := filepath.Join(dir, binDirName, b.Name)
		if err := os.WriteFile(path, []byte(fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return &config.Config{
		ExecutablePath:                  filepath.Join(dir, "runsc"),
		SidecarUsagePolicy:              config.SidecarUsageStrict,
		SidecarReleaseEnforcementPolicy: config.SidecarReleaseAlways,
	}
}

func TestResolverInstallations(t *testing.T) {
	t.Setenv(sidecarBinariesDirEnv, "")
	confs := []*config.Config{resolverInstallation(t, 17), resolverInstallation(t, 23)}
	for _, conf := range confs {
		r := ForConfig(conf)
		for _, b := range All {
			want := filepath.Join(filepath.Dir(conf.ExecutablePath), binDirName, b.Name)
			if got, err := r.Path(b); err != nil || got != want {
				t.Errorf("Path(%s) = %q, %v; want %q", b.Name, got, err, want)
			}
		}
	}
	// Execute real sidecars concurrently, including the checkpoint gofer that
	// is launched after Create has returned to its caller.
	var wg sync.WaitGroup
	for i, conf := range confs {
		wantCode := []int{17, 23}[i]
		r := ForConfig(conf)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				pid, err := r.ForkExec(&CheckpointGofer, Options{})
				if err != nil {
					t.Error(err)
					return
				}
				p, err := os.FindProcess(pid)
				if err != nil {
					t.Error(err)
					return
				}
				st, err := p.Wait()
				if err != nil {
					t.Error(err)
					return
				}
				if st.ExitCode() != wantCode {
					t.Errorf("sidecar exited %d, want installation's %d", st.ExitCode(), wantCode)
				}
			}
		}()
	}
	wg.Wait()
}

func TestResolverPolicies(t *testing.T) {
	t.Setenv(sidecarBinariesDirEnv, "")
	t.Setenv(enforceReleaseEnv, "")
	conf := resolverInstallation(t, 0)
	strict := ForConfig(conf)
	conf.SidecarUsagePolicy = config.SidecarUsageLegacyEmbedded
	conf.SidecarReleaseEnforcementPolicy = config.SidecarReleaseNever
	legacy := ForConfig(conf)

	called := false
	b := &Binary{Name: "missing", embeddedForkExec: func(opts Options) (int, error) {
		called = true
		if !slices.Contains(opts.Envv, enforceReleaseEnv+"=SKIP:"+version.Version()) {
			t.Errorf("fallback did not receive its own release policy: %v", opts.Envv)
		}
		return 123, nil
	}}
	if _, err := strict.ForkExec(b, Options{}); err == nil || called {
		t.Fatal("strict resolver used another runtime's embedded fallback policy")
	}
	if pid, err := legacy.ForkExec(b, Options{}); err != nil || !called || pid != 123 {
		t.Fatalf("legacy fallback: pid=%d err=%v called=%v", pid, err, called)
	}
	input := []string{"KEEP=1", enforceReleaseEnv + "=old"}
	want := []string{"KEEP=1", enforceReleaseEnv + "=" + version.Version()}
	if got := strict.WithEnforceRelease(input); !slices.Equal(got, want) {
		t.Errorf("strict release environment = %v, want %v", got, want)
	}
	if input[1] != enforceReleaseEnv+"=old" {
		t.Error("release enforcement mutated the caller's environment")
	}
}
