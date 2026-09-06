# Kara fork review

This review covers the fork delta from upstream
`602040cbcc584c530ab9eda6b2de142d3161e4b5` to Kara
`f734260e2370c287b291248f6b764202284f3467`: 147 changed files. The upstream
revision is the version integrated into Kara, so newer upstream changes do not
obscure which behavior the fork introduced.

The review is in progress. A checked area requires inspection of its production
changes, relevant callers and tests, and verification of any corrections. Passing
a narrow test does not establish that the remaining areas are complete.

## Coverage

- [ ] Egress transport, TCP classification, UDP enforcement, namespace and restore
      behavior (`runsc/boot`, `pkg/tcpip`).
- [ ] Library API, configuration isolation, executable selection, descriptor
      ownership, lifecycle and error contracts (`runsc/library` and callers).
- [ ] External gofer donation, mount hints, grace periods and CLI validation.
- [ ] Checkpoint cleanup, error classification, task liveness, compatibility keys
      and descriptor restoration.
- [ ] Ingress framing, relay lifecycle, descriptor donation and restore seams.
- [ ] LISAFS wire ABI, generated documentation, golden replay, fuzz coverage and
      external consumer support.
- [ ] Host interfaces, portability stubs, build constraints and platform claims.
- [ ] CI trust boundaries, regression tooling, packaging, command aliases and
      developer documentation.

## Findings under investigation

These are review leads, not claims that every suspected problem has been
reproduced. Each correction will record its regression evidence below.

- Control transport aliases need an audit across guest stdio and explicit guest
  file donations, including egress and external gofer transports.

## Verified corrections

### Egress transport

The client now closes its connection on deadline-setting errors, incomplete
writes, invalid verdicts and request/reply IO failures. A valid denial remains
reusable. Queueing counts toward the two-second call deadline, and explicit
shutdown wakes both queued calls and calls awaiting replies. Loader failure and
shutdown close the owned connection. Donated sockets must be connected Unix
streams; invalid destination lengths are rejected before encoding.

`//runsc/boot:egress_gate_test` reproduces the original deadline/short-write and
invalid-verdict failures and passes after the correction. It also covers queue
expiry without poisoning a healthy connection, shutdown during a check, late
replies, valid denials, packet-socket rejection and donation cleanup. The host
protocol remains version 1, checked against Oca's `network/gate.go`.

Full release, root regressions and GitHub CI passed before PR #12 was merged.
The Oca consumer validation recorded below also includes these changes.

### Egress enforcement after restore

Adding a gate while restoring an ungated checkpoint now persists the requirement
in both the stack and its namespace creator. A second restore without a gate
cannot drop enforcement or create ungated namespaces. A stack restored without
its required gate installs a denying implementation, so callers outside runsc
also fail closed.

Actual `state.Save`/`state.Load` regressions reproduce the original lost marker
and absent gate. The corrected cases pass in
`//pkg/tcpip/tests/integration:egress_gate_test` and
`//runsc/boot:egress_gate_test`, including a second save/restore of the namespace
creator after enforcement was added.

### Bounded TCP prefix capture

Prefix capture now copies a capped packet range without flattening the entire
send queue payload. The original payload is retained for normal TCP delivery.
`//pkg/tcpip/transport/tcp:tcp_test` verifies capture across multiple writes and
that queue data is unchanged.

On the same Linux amd64 host, `BenchmarkEgressL7Prefix` with 200 ms per case
reported the following allocation sizes. These are local measurements, not
cross-machine performance guarantees.

| Queued payload | Before, bytes/op | After, bytes/op |
| --- | ---: | ---: |
| 32 KiB | 65,606 | 32,798 |
| 1 MiB | 1,085,925 | 32,802 |
| 4 MiB | 4,243,054 | 32,783 |

All three affected test targets pass together. Race, release and consumer
validation are tracked separately from these unit and stack integration tests.

### Host interface contracts and build documentation

The host stream interfaces now document partial successful reads/writes and the
need to avoid donating descriptors twice when continuing a partial write. The
ownership contract explicitly leaves an adopted descriptor with the caller on
constructor failure. These statements were checked against
`pkg/unet/unet_unsafe.go` and `unet.NewSocket`.

The README and host-interface package comments distinguish Linux runtime
support from non-Linux compilation scaffolding. They also identify the generated
Go distribution required by external consumers, distinguish that tooling from
an all-Go runtime implementation, and list the previously omitted fork PRs.
These are documentation corrections; they do not add another host backend.

### Library launcher isolation

Concurrent embedded runtimes no longer change `specutils.ExePath`. Each runtime
snapshots its executable path, which is used for gofer and sentry launch and
later checkpoint-gofer launch. A per-configuration sidecar resolver also carries
the sidecar usage and release policies, so one installation cannot reuse the
directory cached for another installation. Existing process-global CLI APIs
remain available.

An eight-runtime concurrent-create regression reproduced the global path race
before the fix. It and `//runsc/gvisorbinaries:gvisorbinaries_test` pass with the
race detector, including concurrent child launches from two fake installations
and independent release/fallback policies.

### Library donation ownership and restore adoption

Create and fresh restore duplicate all donated files with `F_DUPFD_CLOEXEC` and
consume originals only after the whole operation succeeds. Execute and
PortForward use the same ownership transaction and private argument copies.
Cleanup retains the same `os.File` owners throughout lower-level launch, avoiding
raw descriptor closes after a number has been reused. New owned-file forms of
gofer and egress donations retain the legacy raw descriptor API for CLI callers.

Existing-container restore loads its stored spec before consulting a bundle and
rejects create-time donations that the restore RPC cannot replace. Fresh restore
retains all originals if restoration fails after successful container creation.

The unprivileged library target reproduces the previous premature-close failures
and covers partial acquisition, CLOEXEC, descriptor reuse, and adoption without
the original bundle. It passes with the race detector. The full release and root
command, container, and library regression suites pass on systrap and KVM. Root
tests verify successful ownership transfer, failure after sandbox launch, failed
restore, and successful re-donation across a real checkpoint/restore.

Execute and PortForward RPC regressions also pass on both platforms. They verify
remote errors retain originals, successful calls consume them, argument objects
remain unchanged, and guest exec output and bidirectional forwarding continue
through the sandbox's copies.

Oca at `ac61345762ec3ee91c3dc59560b7c94cfa4c0824` passes managed boot,
filesystem workload, extended filesystem operations, loopback, egress denial,
and the egress policy matrix against the release at `82e4b121e`. Its managed
ingress test also passes. Initial fixture failures were resolved by supplying
the expected hostname, installing iproute2 in the test container, and keeping source and
temporary copies on one filesystem for the consumer's hard-link copy helper.
The Oca source tree was unchanged. GitHub CI passed at `82e4b121e`, including
the generated Go consumer, both root platforms, and CodeQL with no new alerts.

### Library configuration and spec isolation

The library now validates and normalizes a private OCI spec before acquiring
files or creating container state. Configuration annotations use the CLI's
override policy with an independent flag set seeded from the runtime's actual
configuration. Each container retains that effective configuration for later
lifecycle calls; Load reconstructs it from the saved spec. Restore compares
the checkpoint platform with the effective annotated platform before creating
a sandbox, while build, CPU and driver mismatches still fail before reading
the spec.

Runtime.Config, Container.Config and Container.Spec return independent
snapshots. New also copies the configuration after MutateConfig returns, so a
retained callback argument cannot change the runtime later.

Regressions reproduced four original failures: mutable runtime configuration,
malformed specs reaching state-directory creation, ignored annotation override
policy, and failed Create mutating the caller's Linux namespaces and mappings.
The corrected config, specutils, binary resolver and unprivileged library
targets pass with the race detector. Coverage includes concurrent annotated
Loads, bundle overrides of nondefault runtime values, deep spec snapshots,
annotated restore compatibility, and the existing donation acquisition and
descriptor reuse regressions.

The updated full release builds, and the command, container and library root
regressions pass on systrap and KVM. The new lifecycle test starts a container
with an annotated host network configuration, checks its normalized private
root path, and verifies that Load retains the effective configuration while
the caller's spec and the runtime's base configuration remain independent.

### Malformed ingress frame regression stability

CI exposed a timing bug in the oversized-length test: the relay correctly
closed after reading the invalid length, while the test sometimes failed with
EPIPE trying to write the unused body. Invalid-length cases now send only the
prefix, verifying that rejection happens without waiting for a body. The
malformed-frame regression passes 1,000 repetitions with the race detector,
and the complete ingress test suite also passes with the race detector.
