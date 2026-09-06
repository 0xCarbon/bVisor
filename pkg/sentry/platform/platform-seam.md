# Platform seam: running the sentry on non-Linux hosts (wave-05)

Status: design + seams landed (wave-05). No non-Linux backend is implemented
yet; this document is the map from the host-coupling inventory (`.wip/`
working notes, mirrored below) to what a macOS (Virtualization.framework)
and Windows (WHP2) backend must provide, and where each requirement plugs
in. It extends `pkg/flipcall/SEAM.md` (wave-03) which specifies the
flipcall host-primitive seams; nothing here changes Linux behavior.

## 1. The interface packages

`platform.Platform`, `platform.Context` and `platform.AddressSpace`
(`platform.go`) are already host-agnostic Go interfaces. All existing
engines — KVM, systrap, ptrace, slimvm — implement them unchanged behind
`platforms/platforms.go`, which is `//go:build linux` (a darwin stub
exists). A new engine registers itself the same way from a per-OS file
(`platforms_darwin.go`, `platforms_windows.go`) — **no sentry-wide ifdefs
are required**. That is the central wave-05 finding: the first-order
blockers are support packages, not the platform interfaces.

The support packages that must compile (and fail closed) on every host, and
their seams:

| Package | Host primitive | Non-Linux stub behavior |
|---|---|---|
| `pkg/fd` | open(2) flags | `oLargeFile` const per OS (0 off-Linux); rest is POSIX |
| `pkg/fdchannel` | SOCK_SEQPACKET + SOCK_CLOEXEC + SCM_RIGHTS pair | socketpair + fcntl(FD_CLOEXEC); `FDDonator` seam (SEAM.md) |
| `pkg/flipcall` | futex + sealed memfd windows | control transfer returns `ErrUnsupported`; layouts pinned host-independent (SEAM.md) |
| `pkg/eventfd` | eventfd(2) | `Create` and I/O return `errors.ErrUnsupported`; descriptor bookkeeping (Wrap/FD/Close/Dup) stays |
| `pkg/seccomp` | prctl(2) seccomp | `SetFilter` → `ErrUnsupported`, `SetFilterInChild` → ENOSYS; BPF building stays portable |
| `pkg/sentry/hostmm` | membarrier(2) | `Probe` reports no support; syscall helpers return ENOSYS |
| `pkg/pinring` | disabled io_uring FD pinning + pidfd exit notification | ring creation, registration and pidfd donation return `errors.ErrUnsupported`; exit notification never claims a confirmed exit |
| `pkg/sentry/platform` | `/proc/sys/vm/mmap_min_addr` init read | reports 0; `MMapMinAddr` embed documented Linux-only |
| `pkg/hostifc` (new) | — | the seam itself: `IPC` factory + `ControlSocket`/stream FD donation + `ControlPlane` probes; `ErrUnsupported` fail-closed backend |

## 2. What a new platform backend must provide

### 2.1 macOS — Virtualization.framework ("vz" backend)

Per oca's specced deployment (oca `spec/04-SANDBOX.md` §macOS VM mode),
production sandboxes on macOS are full vz VMs (virtio-fs + vsock +
gvisor-tap-vsock) and the sentry is NOT the isolation layer there. The seam
work in this repo serves the other mode: hosting the **sentry itself** on
macOS (development, tooling, local compute), where the sentry needs:

- **Execution**: `Context.Switch` needs a CPU context the host can run and
  trap: `Hypervisor.framework` vCPU threads (HV_MEMORY) with exception-based
  syscall interception. The Context/AddressSpace contracts (`Switch`,
  `Interrupt`, `Preempt`, `PullFullState`, `MapFile`/`Unmap` on a
  guest-physical memory file) map 1:1 to what vz/`Hypervisor.framework`
  provides; `Platform.SupportsAddressSpaceIO` should be false initially
  (as KVM) unless a mapping of guest memory into the sentry is established.
- **Memory**: `AddressSpace.MapFile` requires a `memmap.File`-backed guest
  physical memory model (like KVM's physical map, but over
  `Hypervisor.framework` memory regions). `memutil` mmap works on darwin;
  `hostarch` protection bits are portable. `MMapMinAddr` must NOT be
  embedded — the guest address space layout defines `MinUserAddress`.
- **IPC substrate** (`hostifc.IPC`): control sockets are darwin UDS
  (SCM_RIGHTS exists on darwin; `SYS_SENDMSG/RECVMSG` compile), with
  `SOCK_CLOEXEC` emulated via fcntl (done, `pkg/fdchannel`). For the oca
  vsock deployment, a vsock-backed `ControlSocket` is the alternative
  implementation of the same interface — that is exactly why the seam is an
  interface and not `*unet.Socket`.
- **flipcall channels** (SEAM.md §What a wave-05 backend must provide):
  a `Sleeper` on Mach semantics or a POSIX shim on the shared word; a
  `WindowAllocator` over POSIX shm/Mach VM with a truncation-safety
  equivalent of memfd seals; an `FDDonator` (Mach send rights or the
  fdchannel socketpair above). The **16-byte NATIVE-endian packet window
  header** (connState@0, dataLen@4, Data()@16) is part of the LISAFS ABI
  (`pkg/lisafs/ABI.md`) and must not change; both peers read the shared
  window with their native byte order, so a backend pairing an Intel-Mac
  sentry with an arm64 peer is out of contract by construction (same-host
  pairs only).
- **Control plane**: all `ControlPlane` probes are false. Resource limits =
  the VM's memory/CPU configuration; syscall policy = the VM boundary;
  `hostinet` = unavailable (netstack, as oca already does with
  gvisor-tap-vsock); cgroups/memcg pressure = not applicable (pgalloc's
  `UseHostMemcgPressure` must stay off; it already fails closed when the
  opt is unset).

### 2.2 Windows — WHP2

Same interface set; the host-side requirements differ:

- **Execution**: WHP2 (`WHvCreatePartition`/`WHvCreateVp`) provides vCPUs
  with exit-reason-based interception — same shape as vz. Registers via
  `WHvGetVirtualProcessorRegisters`.
- **Memory**: `WHvMapGpaRange`; `memmap.File` backing must come from a
  Windows file mapping (`CreateFileMapping`), i.e. `memutil`'s mmap layer
  needs a Windows backend (`SYS_MMAP` raw calls do not exist there) — this
  is the **known blocking gap** (SEAM.md already flags `pkg/memutil` and
  `pkg/hostarch` page-protection bits as the windows blockers; also
  `pkg/unet`, `pkg/rawfile`, `pkg/fdnotifier` are linux-only today).
- **IPC substrate**: no AF_UNIX-with-SCM_RIGHTS in the required shape; a
  WHP2 backend supplies `hostifc.IPC` over named pipes + a HANDLE-passing
  side channel (or intra-VM vsock/hyper-v sockets). The lisafs consumer
  code is untouched; it is routed through `hostifc` (wave-06).
- **flipcall**: `Sleeper` over a named-pipe/event wait or shared-memory
  condvar; `WindowAllocator` over a file mapping + `SealLocalFile`
  equivalent (or page-file-backed section with fixed size — the
  truncation-safety invariant is the requirement, not the mechanism).
- **Control plane**: identical to macOS — all probes false, VM boundary is
  the sandbox.

## 3. Injection points (where wave-06 plugs in)

1. **Platform registration** — `platforms/platforms_<os>.go` imports the
   new engine packages (vz, whp2). Engines implement `Platform.Constructor`.
2. **hostifc.Default()** — lisafs connection/channel construction moves
   from direct `unet.NewSocket`/`fdchannel.NewConnectedSockets`/
   `flipcall.NewPacketWindowAllocator` calls to `hostifc.Default()`
   (SEAM.md §4, the injection point reserved in wave-03). On Linux this is
   a pure adapter over the unchanged packages.
3. **Control-plane gates** — `hostifc.ProbeControlPlane()` at boot:
   skip seccomp filter installation (`runsc/boot/filter` is launcher-side
   Linux code), disable `UseHostMemcgPressure`, select netstack instead of
   hostinet. The sentry core already compiles with the stubs returning
   `errors.ErrUnsupported`.
4. **Launcher** — `runsc` remains a Linux launcher; a non-Linux host ships
   its own entrypoint (the embeddable library API, wave-06).

## 4. Inventory → backend requirements (condensed)

| Inventory class (`.wip/notes-wave05.md` §Inventory) | macOS vz requirement | Windows WHP2 requirement |
|---|---|---|
| SCM_RIGHTS donation (flipcall channels, lisafs control socket) | darwin UDS SCM_RIGHTS / Mach rights; `hostifc.IPC` | named pipes + HANDLE channel; `hostifc.IPC` |
| UDS substrate (`pkg/unet`) | darwin UDS behind `hostifc.ControlSocket`; raw-syscall waiter needs porting or bypass | hyper-v sockets / named pipes behind `hostifc.ControlSocket` |
| eventfd/timerfd | pipe2+kqueue (or dispatch sem) | event objects |
| poll/epoll (`pkg/fdnotifier`) | kqueue | IOCP |
| raw host file IO (`pkg/rawfile`, `pkg/sentry/hostfd`) | darwin preadv/pwritev | ReadFile/WriteFile overlapped |
| mmap (`pkg/memutil`) | darwin mmap (offset alignment stricter) | VirtualAlloc/CreateFileMapping — **blocking** |
| memfd+seals | POSIX shm + lock | section object fixed size |
| futex | Mach ports / shared-word shim | shared-memory event |
| seccomp filters | n/a (VM boundary) | n/a (VM boundary) |
| cgroups/memcg | VM memory cap | VM memory cap |
| membarrier | not needed (vCPUs) | not needed (vCPUs) |
| namespaces / netlink / hostinet | VM boundary / netstack | VM boundary / netstack |
| /proc reads (`mmap_min_addr`) | guest address space layout | guest address space layout |
| execution engines (kvm/systrap/ptrace/slimvm) | new vz engine | new whp2 engine |

## 5. Proof status (wave-05)

`GOOS=darwin GOARCH=arm64 go build` on the `//:gopath` archive (fresh from
master `a179375a0`) with the wave-05 diff applied, passes for:
`pkg/fd`, `pkg/fdchannel`, `pkg/eventfd`, `pkg/seccomp`, `pkg/flipcall`,
`pkg/memutil`, `pkg/sentry/hostmm`, `pkg/hostifc`, `pkg/sentry/platform`,
`pkg/sentry/platform/interrupt`, `pkg/sentry/platform/platforms`.

Known non-blockers and gaps:

- The archive flattens `pkg/seccomp/precompiledseccomp` (a bazel-generated
  `main` next to the library); plain-`go` imports of it fail identically on
  Linux and darwin — a packaging artifact, not host coupling (bazel builds
  are unaffected; the darwin proof for `pkg/sentry/platform` removes the
  generated main from the copy).
- Windows cross-compilation is blocked earlier and unchanged by this wave:
  `pkg/memutil` (raw `SYS_MMAP`), `pkg/hostarch` page-protection bit
  definitions, `pkg/unet`/`pkg/rawfile`/`pkg/fdnotifier` (all-linux
  packages) — wave-06 gap list.
- `pkg/unet` itself remains Linux-only (raw syscall spelling, `SYS_PPOLL`);
  consumers' needs are covered by `hostifc` interfaces. Porting `unet` or
  routing every call site is wave-06.
- `unix.PollFd`-based watchdogs using `POLLRDHUP` (e.g. lisafs client) need
  a darwin spelling when lisafs is routed through the seam — inventoried,
  wave-06.

Linux invariant: all affected bazel targets stay green (fd, fdchannel,
eventfd, seccomp, hostifc, flipcall, lisafs suite, platform interrupt);
implementations were moved, not modified, except the documented single-token
indirections (`fd.Open` flag const, `fdchannel` socketpair helper).


## Proof methodology notes (review findings)

- The darwin build proofs were run as plain `go build` on the //:gopath
  archive with the wave-05 diff overlaid (the library compiles; in-package
  tests on darwin are future work). Under `bazel build
  --platforms=@io_bazel_rules_go//go/toolchain:darwin_arm64`, all seam
  packages EXCEPT `//pkg/hostifc` also cross-compile green; `pkg/hostifc`
  depends on `//pkg/unet` unconditionally in its BUILD file, and
  `pkg/unet/unet_unsafe.go` does not compile on darwin (Cmsghdr/Iovec
  width mismatches). Wave-06 must either tag-split the unet dep or route
  through a build-tag-selected adapter before bazel-darwin CI can include
  `pkg/hostifc`.
- The socketpair stub keeps SOCK_SEQPACKET for compile shape; macOS AF_UNIX
  does not implement SOCK_SEQPACKET (XNU uipc supports STREAM/DGRAM only),
  so a darwin backend must substitute SOCK_DGRAM at the ipc adapter layer.
- `platforms.go` uses the build tag `linux && !debug` (not bare `linux`).
- SEAM.md's injection point is item 4 of the "What a wave-05 backend must
  provide" list (the document has no numbered sections).
