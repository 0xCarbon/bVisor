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
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDonationCleanupAfterDescriptorReuse(t *testing.T) {
	original, err := os.CreateTemp(t.TempDir(), "original")
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	var donations fileDonations
	defer donations.close()
	copy, err := donations.copy(original)
	if err != nil {
		t.Fatal(err)
	}
	fd := copy.Fd()
	flags, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("donation can leak into unrelated children: flags=%d err=%v", flags, err)
	}
	// Model a launch path that has already consumed its copy, followed by
	// unrelated work opening a file before error cleanup runs.
	copy.Close()
	replacement, err := os.Open(original.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if replacement.Fd() != fd {
		t.Skip("a concurrent process operation claimed the available descriptor")
	}
	donations.close()
	if _, err := replacement.Stat(); err != nil {
		t.Fatalf("cleanup closed a reused descriptor: %v", err)
	}
	if _, err := original.Stat(); err != nil {
		t.Fatalf("cleanup consumed the original before commit: %v", err)
	}
	donations.commit()
	if _, err := original.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("commit did not consume the caller's original: %v", err)
	}
}
