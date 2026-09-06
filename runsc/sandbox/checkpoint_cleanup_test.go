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

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/state/statefile"
)

// TestCreateSaveFilesPartialFailureCleanup verifies that a createSaveFiles()
// call that fails partway (here: the pages metadata file collides with a
// pre-existing file) removes the files it already created instead of leaving
// a half-created image behind. A leftover would both break a same-directory
// retry (O_EXCL) and let a restore consume a truncated image.
//
// This is the deterministic core of the checkpoint-image-truncation
// regression (wave-04): it fails on trees without the cleanup.
func TestCreateSaveFilesPartialFailureCleanup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		compression statefile.CompressionLevel
	}{
		{"none", statefile.CompressionLevelNone},
		{"flate-best-speed", statefile.CompressionLevelFlateBestSpeed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Simulate a collision with the second file createSaveFiles()
			// attempts. With compression "none" that is pages_meta.img; with
			// compression there is no second file, so collide on the only
			// other file the flate path creates (none) and instead collide on
			// the state file itself for the compressed case.
			collide := checkpointfiles.PagesMetadataFileName
			if tc.compression != statefile.CompressionLevelNone {
				collide = checkpointfiles.StateFileName
			}
			if err := os.WriteFile(filepath.Join(dir, collide), []byte("old"), 0644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			files, err := createSaveFiles(dir, false /* direct */, tc.compression)
			if err == nil {
				for _, f := range files {
					_ = f.Close()
				}
				t.Fatalf("createSaveFiles unexpectedly succeeded")
			}
			// The colliding file belongs to someone else and must survive.
			if _, statErr := os.Stat(filepath.Join(dir, collide)); statErr != nil {
				t.Errorf("pre-existing file %q was removed: %v", collide, statErr)
			}
			// Any file created by the failed attempt must be gone.
			for _, name := range []string{checkpointfiles.StateFileName, checkpointfiles.PagesMetadataFileName, checkpointfiles.PagesFileName} {
				if name == collide {
					continue
				}
				if _, statErr := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(statErr) {
					t.Errorf("file %q left behind by failed createSaveFiles()", name)
				}
			}
		})
	}
}

// TestCreateSaveFilesSuccess creates the expected files and transfers
// ownership to the caller (no cleanup must fire on success).
func TestCreateSaveFilesSuccess(t *testing.T) {
	dir := t.TempDir()
	files, err := createSaveFiles(dir, false /* direct */, statefile.CompressionLevelNone)
	if err != nil {
		t.Fatalf("createSaveFiles: %v", err)
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	for _, name := range []string{checkpointfiles.StateFileName, checkpointfiles.PagesMetadataFileName, checkpointfiles.PagesFileName} {
		if _, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil {
			t.Errorf("expected file %q missing: %v", name, statErr)
		}
	}
}

// TestRemoveLocalSaveFiles verifies the exact-set property of the
// checkpoint-failure cleanup: only the files createSaveFiles() would have
// created for the given options are removed; everything else in the image
// directory is left alone.
func TestRemoveLocalSaveFiles(t *testing.T) {
	unrelated := "user-notes.txt"
	for _, tc := range []struct {
		name        string
		compression statefile.CompressionLevel
	}{
		{"none", statefile.CompressionLevelNone},
		{"flate-best-speed", statefile.CompressionLevelFlateBestSpeed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			want := map[string]bool{unrelated: true, checkpointfiles.StateFileName: false}
			if tc.compression == statefile.CompressionLevelNone {
				want[checkpointfiles.PagesMetadataFileName] = false
				want[checkpointfiles.PagesFileName] = false
			}
			// A filesystem snapshot belongs to the caller, not this checkpoint.
			// It must survive alongside unrelated top-level files.
			want[filepath.Join("fs", checkpointfiles.PagesFileName)] = true
			if err := os.Mkdir(filepath.Join(dir, "fs"), 0755); err != nil {
				t.Fatalf("Mkdir: %v", err)
			}
			for name := range want {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("stub"), 0644); err != nil {
					t.Fatalf("WriteFile(%q): %v", name, err)
				}
			}
			opts := CheckpointOpts{Compression: tc.compression}
			if err := removeLocalSaveFiles(dir, opts); err != nil {
				t.Fatalf("removeLocalSaveFiles: %v", err)
			}
			for name, keep := range want {
				_, statErr := os.Stat(filepath.Join(dir, name))
				if keep && statErr != nil {
					t.Errorf("file %q must survive the cleanup: %v", name, statErr)
				}
				if !keep && !os.IsNotExist(statErr) {
					t.Errorf("file %q must be removed by the cleanup", name)
				}
			}
		})
	}
}
