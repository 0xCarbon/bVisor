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

// Package compat defines the canonical restore-compatibility key for
// checkpoint images (wave-04).
//
// A checkpoint image is only restorable by a compatible runsc build on a
// compatible host: the save format is build-specific, the saved kernel
// embeds a sentry platform, and restoring onto a host lacking the CPU
// features the image was saved under fails. Embedders that schedule
// restores (e.g. oca's SUSPENDED tier) previously hand-rolled this key
// from `runsc --version` plus a CPU-features annotation. This package is
// the canonical composition so the key has one definition and one
// serialization:
//
//		v1|<runsc build>|<platform>|<cpu-features-id>|<driver>
//
//	  - v1 is the key format version; a future incompatible format bumps it.
//	  - runsc build is runsc/version.Version() (restore also enforces this
//	    server-side: runsc/boot/controller.go rejects version mismatches).
//	  - platform is the sentry platform name (systrap, kvm, ...).
//	  - cpu-features-id is a stable 16-hex-character digest of the canonical
//	    host CPU feature list (the same list `runsc cpu-features` prints, in
//	    the same order). The digest keeps the serialized key small enough for
//	    placement metadata budgets (~64 bytes total); the full list remains
//	    available through `runsc cpu-features`. The digest changes
//	    conservatively when the feature set changes.
//	  - driver optionally pins a host GPU driver (e.g. the NVIDIA driver
//	    branch for cuda-checkpoint images); it is empty for CPU sandboxes and
//	    is caller-supplied (runsc does not probe the GPU driver host-side).
//
// Fields never contain the '|' separator. Empty trailing fields serialize
// as empty strings and compare equal only to empty.
package compat

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/runsc/version"
)

// KeyPrefix is the current serialized key format version.
const KeyPrefix = "v1"

// Key is the restore-compatibility class of a checkpoint image or host.
type Key struct {
	// Build is the runsc build identifier that wrote (or would write) the
	// image: runsc/version.Version().
	Build string

	// Platform is the sentry platform the sandbox runs on (e.g. "systrap").
	Platform string

	// CPUFeaturesID is the stable digest of the canonical CPU feature list
	// (CPUFeaturesID()); empty compares equal only to empty.
	CPUFeaturesID string

	// Driver optionally pins the host GPU driver class of the image (e.g.
	// "570" from "570.86.15"); empty for CPU sandboxes.
	Driver string
}

// String serializes the key as "v1|build|platform|cpufeatures|driver".
func (k Key) String() string {
	return strings.Join([]string{KeyPrefix, k.Build, k.Platform, k.CPUFeaturesID, k.Driver}, "|")
}

// Validate reports whether the key is well-formed: no field may contain the
// '|' separator.
func (k Key) Validate() error {
	for name, f := range map[string]string{
		"Build":         k.Build,
		"Platform":      k.Platform,
		"CPUFeaturesID": k.CPUFeaturesID,
		"Driver":        k.Driver,
	} {
		if strings.ContainsRune(f, '|') {
			return fmt.Errorf("compat.Key field %s must not contain %q: %q", name, '|', f)
		}
	}
	return nil
}

// Compatible reports whether an image saved under key image can be restored
// under key k: every field must match exactly (missing information is not
// compatibility).
func (k Key) Compatible(image Key) bool {
	return k.Build == image.Build && k.Platform == image.Platform &&
		k.CPUFeaturesID == image.CPUFeaturesID && k.Driver == image.Driver
}

// Parse is the inverse of Key.String.
func Parse(s string) (Key, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 5 {
		return Key{}, fmt.Errorf("malformed compat key %q (want %s|build|platform|cpufeatures|driver)", s, KeyPrefix)
	}
	if parts[0] != KeyPrefix {
		return Key{}, fmt.Errorf("unsupported compat key version %q (want %q)", parts[0], KeyPrefix)
	}
	k := Key{Build: parts[1], Platform: parts[2], CPUFeaturesID: parts[3], Driver: parts[4]}
	if err := k.Validate(); err != nil {
		return Key{}, err
	}
	return k, nil
}

// CanonicalCPUFeatures returns the canonical host CPU feature list: the
// features from cpuid.AllFeatures() present on this host, in canonical
// (enum) order, comma-joined. `runsc cpu-features` prints exactly this
// string; the two must not drift (the command calls this function).
func CanonicalCPUFeatures() string {
	cpuid.Initialize()
	hfs := cpuid.HostFeatureSet().Fixed()
	var features []string
	for _, v := range cpuid.AllFeatures() {
		if hfs.HasFeature(v) {
			features = append(features, v.String())
		}
	}
	// Restore's FeatureSet.CheckHostCompatible also compares CacheLine()
	// (pkg/cpuid/cpuid_amd64.go), which is not a Boolean feature; include
	// it so two hosts with identical feature lists but different cache-line
	// sizes produce different compatibility keys.
	features = append(features, fmt.Sprintf("cache_line_size=%d", cpuid.HostFeatureSet().CacheLine()))
	return strings.Join(features, ",")
}

// CPUFeaturesID returns a stable, compact identity of CanonicalCPUFeatures():
// the first 16 hex characters of its SHA-256 digest. Identical lists map to
// identical IDs; any change to the list changes the ID (conservative
// incompatibility).
func CPUFeaturesID() string {
	sum := sha256.Sum256([]byte(CanonicalCPUFeatures()))
	return hex.EncodeToString(sum[:8])
}

// HostKey composes the restore-compatibility key for this host under the
// given sentry platform. driver optionally pins a GPU driver class; pass ""
// for CPU sandboxes.
func HostKey(platform, driver string) Key {
	return Key{
		Build:         version.Version(),
		Platform:      platform,
		CPUFeaturesID: CPUFeaturesID(),
		Driver:        driver,
	}
}
