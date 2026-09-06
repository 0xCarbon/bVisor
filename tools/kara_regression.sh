#!/bin/bash

# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# kara_regression.sh runs the fork's command, container, and library
# regression suites against a bazel-built release, including sidecars.
#
# The container test binaries need two things bazel's sandbox does not
# provide: root (sandboxes, cgroups, PID namespaces) and a runsc binary in
# a "runfiles-like" layout - testutil.ConfigureExePath runs BEFORE
# flag.Parse, so the -runsc flag never applies outside bazel; it searches
# for release/runsc and release/gvisor-bin below a "_main" directory.
# Copy the complete release there and run the compiled tests as root.
#
# Usage:
#   tools/kara_regression.sh [-p platforms] [-r test-regex] [-t tmpdir]
#     -p platforms   comma-separated platform list passed to the container
#                    tests via their -test_platforms flag (default: systrap;
#                    empty uses all container platforms and the library default)
#     -r test-regex  -test.run filter for the container test binary
#                    (default: the fork-added regression subset)
#     -t tmpdir      scratch directory (default: mktemp)
#
# Environment: bazel (or bazelisk) on PATH; sudo for the test run itself.

set -euo pipefail

PLATFORMS="systrap"
CONTAINER_RUN='TestCheckpointCleansImageWhenSandboxDies|TestCheckpointRestoreBackToBack|TestCheckpointRestoreLongSleep|TestCheckpointRestorePassFDDonation|TestCheckpointRestoreSignalHandlerRead|TestPortForwardDialProbe|TestSignalUserspaceSpinTask|TestStateAfterInitKilled'
TMPDIR_ARG=""

while getopts "p:r:t:" opt; do
  case "${opt}" in
    p) PLATFORMS="${OPTARG}" ;;
    r) CONTAINER_RUN="${OPTARG}" ;;
    t) TMPDIR_ARG="${OPTARG}" ;;
    *) echo "usage: $0 [-p platforms] [-r test-regex] [-t tmpdir]" >&2; exit 2 ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

WORK="${TMPDIR_ARG:-$(mktemp -d /tmp/kara-regression.XXXXXX)}"
mkdir -p "${WORK}"
# The non-DirectFS posture tests exec runsc as nobody. mktemp defaults to
# mode 0700; allow traversal to the release without exposing directory listings.
chmod a+x "${WORK}"
echo "==> workspace: ${WORK}"

echo "==> building release and test binaries (bazel)"
bazel build //:release //runsc/cmd:cmd_test //runsc/container:container_test //runsc/library:library_test

echo "==> laying out the _main runfiles shape"
mkdir -p "${WORK}/rt/_main/release"
cp -a bazel-bin/release/. "${WORK}/rt/_main/release/"
cp -f bazel-bin/runsc/cmd/cmd_test_/cmd_test "${WORK}/cmd.test"
cp -f bazel-bin/runsc/container/container_test_/container_test "${WORK}/container.test"
cp -f bazel-bin/runsc/library/library_test_/library_test "${WORK}/library.test"
chmod +x "${WORK}"/cmd.test "${WORK}"/container.test "${WORK}"/library.test

mkdir -p "${WORK}/tmp"
cd "${WORK}/rt/_main"

echo "==> command regression suite"
sudo env TEST_TMPDIR="${WORK}/tmp" \
  "${WORK}/cmd.test" -test.v -test.timeout 1200s

echo "==> container regression subset (platforms: ${PLATFORMS})"
sudo env TEST_TMPDIR="${WORK}/tmp" \
  "${WORK}/container.test" -test.v -test.timeout 3600s \
  -test_platforms="${PLATFORMS}" -test.run "${CONTAINER_RUN}"

echo "==> library reference embedder suite (platforms: ${PLATFORMS})"
if [ -z "${PLATFORMS}" ]; then
  LIBRARY_PLATFORMS=("")
else
  IFS=',' read -r -a LIBRARY_PLATFORMS <<< "${PLATFORMS}"
fi
for library_platform in "${LIBRARY_PLATFORMS[@]}"; do
  sudo env TEST_TMPDIR="${WORK}/tmp" \
    "${WORK}/library.test" -test.v -test.timeout 1200s \
    -library-platform="${library_platform}"
done

echo "==> PASS"
