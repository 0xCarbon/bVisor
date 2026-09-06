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

package container

import (
	"fmt"
	"os"
)

// prepareDonations gives each legacy raw descriptor one os.File owner for the
// entire creation path. Later cleanup must close that owner, not the descriptor
// number, which a failed launch may already have closed and reused.
func (args *Args) prepareDonations() error {
	if len(args.IOFDs) != 0 && len(args.IOFiles) != 0 {
		return fmt.Errorf("IOFDs and IOFiles cannot both be set")
	}
	if args.EgressFD != nil && args.EgressFile != nil {
		return fmt.Errorf("EgressFD and EgressFile cannot both be set")
	}
	if args.IngressFD != nil && args.IngressFile != nil {
		return fmt.Errorf("IngressFD and IngressFile cannot both be set")
	}
	for i, f := range args.IOFiles {
		if f == nil || int(f.Fd()) < 0 {
			return fmt.Errorf("invalid gofer IO file %d", i)
		}
	}
	for i, fd := range args.IOFDs {
		if fd < 0 {
			return fmt.Errorf("invalid gofer IO FD %d: %d", i, fd)
		}
	}
	if args.EgressFD != nil && *args.EgressFD < 0 {
		return fmt.Errorf("invalid egress FD %d", *args.EgressFD)
	}
	if args.IngressFD != nil && *args.IngressFD < 0 {
		return fmt.Errorf("invalid ingress FD %d", *args.IngressFD)
	}
	owners := make(map[int]*os.File)
	adopt := func(fd int, name string) *os.File {
		if f := owners[fd]; f != nil {
			return f
		}
		f := os.NewFile(uintptr(fd), name)
		owners[fd] = f
		return f
	}
	for _, fd := range args.IOFDs {
		args.IOFiles = append(args.IOFiles, adopt(fd, "gofer-io-fd"))
	}
	if args.EgressFD != nil {
		args.EgressFile = adopt(*args.EgressFD, "egress-fd")
	}
	if args.IngressFD != nil {
		args.IngressFile = adopt(*args.IngressFD, "ingress-fd")
	}
	return nil
}

func (args *Args) closeDonations() {
	closeFiles(args.IOFiles...)
	closeFiles(args.EgressFile, args.IngressFile)
}

func closeFiles(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			f.Close()
		}
	}
}
