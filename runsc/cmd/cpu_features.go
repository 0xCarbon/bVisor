// Copyright 2025 The gVisor Authors.
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

package cmd

import (
	"context"
	"fmt"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/compat"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
)

// CPUFeatures implements subcommands.Command for the "cpu-features" command.
type CPUFeatures struct {
	// compatKey prints the canonical restore-compatibility key instead of
	// the raw feature list.
	compatKey bool

	// compatDriver supplies the optional GPU driver component of the
	// compatibility key (e.g. a pinned NVIDIA driver branch); empty for CPU
	// sandboxes. runsc does not probe the GPU driver host-side.
	compatDriver string
}

// Name implements subcommands.command.name.
func (*CPUFeatures) Name() string {
	return "cpu-features"
}

// Synopsis implements subcommands.Command.Synopsis.
func (*CPUFeatures) Synopsis() string {
	return "list CPU features supported on current machine"
}

// Usage implements subcommands.Command.Usage.
func (*CPUFeatures) Usage() string {
	return "cpu-features\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (c *CPUFeatures) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&c.compatKey, "compat-key", false, "print the canonical restore-compatibility key (v1|build|platform|cpu-features-id|driver) instead of the feature list")
	f.StringVar(&c.compatDriver, "compat-driver", "", "GPU driver component for --compat-key (e.g. the pinned NVIDIA driver branch); empty for CPU sandboxes")
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (*CPUFeatures) FetchSpec(_ *config.Config, _ *flag.FlagSet) (string, *specs.Spec, error) {
	// This command does not operate on a single container, so nothing to fetch.
	return "", nil, nil
}

// Execute implements subcommands.Command.Execute.
func (c *CPUFeatures) Execute(_ context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if c.compatKey {
		conf, ok := args[0].(*config.Config)
		if !ok {
			util.Fatalf("missing config")
		}
		fmt.Println(compat.HostKey(conf.Platform, c.compatDriver).String())
		return subcommands.ExitSuccess
	}
	// The canonical feature list lives in one place so the compatibility
	// key's digest and this output cannot drift.
	fmt.Println(compat.CanonicalCPUFeatures())
	return subcommands.ExitSuccess
}
