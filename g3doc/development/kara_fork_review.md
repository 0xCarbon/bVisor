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

- `Runtime.ExePath` temporarily changes a global executable path; sidecar lookup
  caches the first installation globally. Concurrent and sequential runtimes
  need independent launcher selection.
- Library gofer and egress donations close caller files before success and use
  raw descriptor cleanup that can outlive the owning `os.File`. Other donations
  need the same ownership audit.
- Library spec annotations use global flag registration/configuration paths,
  and restore reads the bundle even when restoring an existing container.

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

Full release and consumer validation remain pending until the broader changes
are ready.

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
