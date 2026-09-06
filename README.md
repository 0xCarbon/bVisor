<div align="center">

# Kara

**The sandbox kernel for [`oca`](https://github.com/0xCarbon/oca) — a [`gVisor`](https://github.com/google/gvisor) fork with runtime integrations and groundwork for future host ports.**

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
for the [`oca`](https://github.com/0xCarbon/oca) agent runtime**. Running
sandboxes currently requires **Linux**. Interfaces and compilation stubs prepare
selected packages for future macOS and Windows backends; those backends and
their isolation boundaries are not implemented.

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
- **Go module consumer support** &mdash; `pkg/lisafs`, `pkg/flipcall` and
  `pkg/fdchannel` are importable from the generated Go source distribution,
  with the wire ABI frozen and machine-checked (third-party gofer
  enablement).
- **Library API** &mdash; `runsc/library` exposes typed lifecycle options,
  checkpoint errors and compatibility keys to Go embedders.
- **Host ingress** (`--ingress-fd`) &mdash; multiplexed TCP relaying into the
  sandbox netstack over a donated Unix stream, with lifecycle and restore tests.
- **Host interfaces** *(in progress)* &mdash; Linux adapters, non-Linux stubs
  and selected Darwin compilation checks. Capability descriptions do not replace
  kernel or permission checks, and LISAFS still uses its concrete Linux paths.

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

Go module consumers use the generated `go` branch or the sources in Bazel's
`//:gopath` archive. These distributions include the generated files needed by
ordinary `go build`; a raw `master` checkout requires Bazel to generate them.
The [external consumer check](pkg/lisafs/plain_go_import_test.sh) documents the
archive layout and verifies the LISAFS, flipcall and fdchannel import surface.

Here, Go module support describes the consumer's build tooling. The full runtime
also includes architecture-specific assembly and native signal-handling code;
it is not implemented entirely in Go. Compiling selected packages for Darwin
does not provide a working Darwin sandbox runtime.

For gVisor's full user, installation, and debugging documentation, see
[`g3doc/`](g3doc/) (inherited from upstream).

## Fork delta

| Area | What changed | Landed as |
|------|--------------|-----------|
| `runsc/container` | C/R signal-handler & task-liveness regression tests (gvisor#14139 investigation) | PR #1 |
| `runsc/sandbox` | zombie-aware `IsRunning` (state settles after init death) | PR #2 |
| netstack + runsc | external gofer `--io-fds`, gofer-monitor grace, egress flow gate `--egress-fd` (+ restore-survival, fail-closed config validation) | PR #3 |
| `pkg/lisafs`, `pkg/flipcall` | plain-Go surface, frozen wire ABI + fuzz/golden conformance, host-primitive seam | PR #4 |
| checkpoint/restore | cleanup, typed errors, compatibility keys and descriptor donation contracts | PR #5 |
| host interfaces | Linux adapters, non-Linux stubs and compilation checks | PR #7 |
| library + CI | typed runtime API, reference embedder and fork regression workflows | PR #8 |
| upstream integration | updated runtime packaging and matching sidecar release builds | PR #9 |
| ingress | donated-FD relay, half-closes, lifecycle and restore ownership | PR #10, #11 |

The [fork review](g3doc/development/kara_fork_review.md) tracks ongoing runtime,
interface, security, performance and developer-experience corrections.

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
