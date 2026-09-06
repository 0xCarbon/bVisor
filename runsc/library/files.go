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
	"os"
	"runtime"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/container"
)

// fileDonations owns private copies until the operation finishes. Originals
// remain caller-owned on every failure path and are consumed only by commit.
type fileDonations struct {
	copies    []*os.File
	originals map[*os.File]struct{}
}

func (d *fileDonations) copy(f *os.File) (*os.File, error) {
	if f == nil {
		return nil, nil
	}
	dup, err := duplicateFile(f)
	if err != nil {
		return nil, err
	}
	d.copies = append(d.copies, dup)
	if d.originals == nil {
		d.originals = make(map[*os.File]struct{})
	}
	d.originals[f] = struct{}{}
	return dup, nil
}

func (d *fileDonations) copyFiles(files []*os.File) ([]*os.File, error) {
	if len(files) == 0 {
		return nil, nil
	}
	copies := make([]*os.File, len(files))
	for i, f := range files {
		if f == nil {
			return nil, fmt.Errorf("file %d is nil", i)
		}
		dup, err := d.copy(f)
		if err != nil {
			return nil, fmt.Errorf("file %d: %w", i, err)
		}
		copies[i] = dup
	}
	return copies, nil
}

func (d *fileDonations) acquire(args *container.Args) error {
	var err error
	if args.IOFiles, err = d.copyFiles(args.IOFiles); err != nil {
		return fmt.Errorf("acquiring gofer IO files: %w", err)
	}
	if args.EgressFile, err = d.copy(args.EgressFile); err != nil {
		return fmt.Errorf("acquiring egress file: %w", err)
	}
	if args.IngressFile, err = d.copy(args.IngressFile); err != nil {
		return fmt.Errorf("acquiring ingress file: %w", err)
	}
	if args.ExecFile, err = d.copy(args.ExecFile); err != nil {
		return fmt.Errorf("acquiring executable file: %w", err)
	}
	if len(args.PassFiles) != 0 {
		copies := make(map[int]*os.File, len(args.PassFiles))
		for guestFD, f := range args.PassFiles {
			if guestFD < 0 || f == nil {
				return fmt.Errorf("invalid file donation for guest FD %d", guestFD)
			}
			dup, err := d.copy(f)
			if err != nil {
				return fmt.Errorf("acquiring guest FD %d: %w", guestFD, err)
			}
			copies[guestFD] = dup
		}
		args.PassFiles = copies
	}
	return nil
}

func (d *fileDonations) close() {
	for _, f := range d.copies {
		f.Close()
	}
}

func (d *fileDonations) commit() {
	for f := range d.originals {
		f.Close()
	}
}

// duplicateFile borrows f and returns a separately owned donation. CLOEXEC
// prevents inheritance by unrelated children created during container setup.
func duplicateFile(f *os.File) (*os.File, error) {
	if f == nil {
		return nil, nil
	}
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	runtime.KeepAlive(f)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), f.Name()), nil
}
