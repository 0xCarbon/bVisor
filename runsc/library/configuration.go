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
	"path/filepath"

	"github.com/mohae/deepcopy"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/specutils"
)

func copySpec(spec *specs.Spec) *specs.Spec {
	if spec == nil {
		return nil
	}
	return deepcopy.Copy(spec).(*specs.Spec)
}

// forSpec creates the immutable configuration used by one container's entire
// lifecycle. The caller's runtime and process-global flag values are untouched.
func (r *Runtime) forSpec(spec *specs.Spec) (*Runtime, error) {
	conf := r.conf.Clone()
	if spec != nil && len(spec.Annotations) != 0 {
		fs := flag.NewFlagSet("runsc-library-container", flag.ContinueOnError)
		config.RegisterFlags(fs)
		// ApplyBundles compares against the flag set's current values. Seed
		// it with this runtime's configuration, including MutateConfig edits.
		if err := fs.Parse(conf.ToFlags()); err != nil {
			return nil, fmt.Errorf("library: initializing configuration flags: %w", err)
		}
		if err := specutils.FixConfigWithFlagSet(conf, spec, fs); err != nil {
			return nil, fmt.Errorf("library: applying configuration annotations: %w", err)
		}
	}
	return &Runtime{conf: conf}, nil
}

func (r *Runtime) prepareSpec(spec *specs.Spec, bundle string) (*Runtime, *specs.Spec, string, error) {
	bundle, err := filepath.Abs(bundle)
	if err != nil {
		return nil, nil, "", fmt.Errorf("library: resolving bundle directory: %w", err)
	}
	owned := copySpec(spec)
	// Validate before creating container state or acquiring donated resources.
	if err := specutils.PrepareSpec(owned, bundle, r.conf); err != nil {
		return nil, nil, "", fmt.Errorf("library: preparing spec: %w", err)
	}
	rt, err := r.forSpec(owned)
	return rt, owned, bundle, err
}

func (r *Runtime) adopt(c *container.Container) (*Container, error) {
	rt, err := r.forSpec(c.Spec)
	if err != nil {
		return nil, err
	}
	return &Container{rt: rt, cont: c}, nil
}
