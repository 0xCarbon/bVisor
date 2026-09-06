<div align="center">

# Kara

**The sandbox kernel for [`oca`](https://github.com/0xCarbon/oca) — a [`gVisor`](https://github.com/google/gvisor) fork extended beyond Linux-only hosts.**

*Kara — Tupi-Guarani for skin, bark, husk: the layer that wraps and protects what lives inside.*

<p>
  <a href="https://github.com/google/gvisor">
    <img src="https://img.shields.io/badge/fork%20of-google%2Fgvisor-blue" alt="fork of google/gvisor">
  </a>
  &nbsp;
  <a href="LICENSE">
    <img src="https://img.shields.io/badge/license-Apache--2.0-green" alt="Apache-2.0">
  </a>
</p>

</div>

<br />

## Overview

Kara is 0xCarbon's fork of [gVisor](https://github.com/google/gvisor), the
user-space kernel that implements the Linux system surface and isolates
workloads from the host. The fork's mission: serve as the **sandbox kernel
for the [`oca`](https://github.com/0xCarbon/oca) agent runtime** — and keep
being that kernel on **macOS and Windows**, not only Linux, by lifting the
host-platform seam instead of forking the sentry with `#ifdef`s.

Upstream merges land regularly; the fork delta is kept minimal, reviewed, and
rebasable (see [Fork delta](#fork-delta) and
[Upstream policy](#upstream-policy)).

### Key features

- **Egress flow gate** (`--egress-fd`) &mdash; pre-route TCP/UDP admission with
  bounded L7 prefix mirroring, a fail-closed sentry-side client speaking a
  frozen wire format, and enforcement that **survives checkpoint/restore**
  across every network namespace.
- **External gofer** (`--io-fds`) &mdash; create/run/restore accept donated
  lisafs connections so an embedder can serve the sandbox filesystem from its
  own gofer process.
- **Gofer-monitor grace** &mdash; bounded 2 s grace with clean-exit detection
  before SIGKILL on gofer disconnect.
- **C/R contract hardening** &mdash; zombie-aware sandbox liveness, restore
  wedge regressions pinned by tests, held egress flows terminate at restore
  (never resume unclassified).
- **Plain-Go `pkg/lisafs` + `pkg/flipcall`** &mdash; importable without bazel,
  with the wire ABI frozen and machine-checked (third-party gofer
  enablement).
- **Host-platform seam** *(in progress)* &mdash; Linux-only control plane made
  optional behind capability probes so Hypervisor.framework and WHP backends
  can land without sentry-wide conditionals.

## Kara and oca

[`oca`](https://github.com/0xCarbon/oca) is the agent sandbox runtime that
consumes Kara: it drives `runsc` with donated gofer FDs and an egress gate,
and suspends/resumes agents through checkpoint/restore. Oca owns the agent
lifecycle; Kara owns the kernel it runs on. The end state: **oca builds
against Kara master with zero ocadiff patches**.

## Getting started

Kara builds like gVisor. The quick path:

```bash
git clone https://github.com/0xCarbon/kara
cd kara

# bazel (authoritative; regenerates stateify autogen)
bazel build //:release

# Keep runsc and its matching gvisor-bin/ sidecars together.
export PATH="$PWD/bazel-bin/release:$PATH"

# run a sandbox with the egress gate + an external gofer
runsc --network=sandbox --overlay2=none --directfs=false \
  create -bundle /path/to/bundle --io-fds=3 --egress-fd=4 my-sandbox
```

Plain-Go consumers of `pkg/lisafs` / `pkg/flipcall` need no bazel at all —
see the packages' doc headers for the frozen ABI contract.

For gVisor's full user, installation, and debugging documentation, see
[`g3doc/`](g3doc/) (inherited from upstream).

## Fork delta

| Area | What changed | Landed as |
|------|--------------|-----------|
| `runsc/container` | C/R signal-handler & task-liveness regression tests (gvisor#14139 investigation) | PR #1 |
| `runsc/sandbox` | zombie-aware `IsRunning` (state settles after init death) | PR #2 |
| netstack + runsc | external gofer `--io-fds`, gofer-monitor grace, egress flow gate `--egress-fd` (+ restore-survival, fail-closed config validation) | PR #3 |
| `pkg/lisafs`, `pkg/flipcall` | plain-Go surface, frozen wire ABI + fuzz/golden conformance, host-primitive seam | PR #4 |

Roadmap: checkpoint/restore contract API hardening, the full host-platform
seam lift (macOS/Windows pre-work), and embeddable library mode + the oca
managed-e2e suite as CI gates.

## Upstream policy

- `upstream` = `google/gvisor` `master`; syncs are merge commits, gated by
  `bazel build //:release` plus the fork's test targets before push.
- The fork never rewinds upstream behavior: fork features fail closed and
  stay no-ops when their flags are unset (nil gate / no `--io-fds` = stock).
- Wire formats and ABI surfaces the fork introduces (`EgressGate` protocol,
  LISAFS ABI) are contractual and frozen.

## Contributing

Small, reviewable, wave-shaped changes. See
[CONTRIBUTING.md](CONTRIBUTING.md) (inherited) for the basics; open an issue
first for anything that touches the sentry or the platform seam.

## Security

See [SECURITY.md](SECURITY.md). Kara inherits gVisor's attack-surface
posture and adds its own rule: **sandbox egress enforcement must fail
closed** — an unconfigurable or unrestorable gate is a boot or restore
failure, never a silent pass-through.

## Code of conduct

[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) (inherited).

## Acknowledgements

Kara stands on [gVisor](https://github.com/google/gvisor) and its community.
The name follows 0xCarbon's indigenous-Brazilian naming — *kene, taba, ajuri,
oca, aba, kara*.
