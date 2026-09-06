# Host ingress over a donated socket

Kara can carry host-accepted TCP connections into the sandbox netstack. The host
owns the listeners and chooses the destination address and port for each
connection. This works with `--network=sandbox` and `--network=none` (loopback
only); host and plugin networking are rejected.

Donate one end of a connected Unix stream socket using `--ingress-fd=N` on
`runsc create`, `runsc run`, or `runsc restore`. The default, `-1`, disables the
relay. Descriptor zero is valid. If the donation aliases a standard descriptor,
that application stdio stream is replaced with `/dev/null`: the relay transport
must not be readable or writable by the application.

Embedders use `library.CreateOptions.IngressFile` and
`library.RestoreOptions.IngressFile`. A successful call consumes the file; a
failed call leaves the caller's file open and closes the runtime's duplicates.
The lower-level `container.Args.IngressFile` transfers ownership on every return.
Its older `IngressFD` field remains available for source compatibility; do not
set both fields. `container.RestoreWithOptions` and `sandbox.RestoreWithOptions`
borrow their ingress file for the duration of the call.

At checkpoint, finish or cancel carried connections and close the host transport.
The relay itself is not serialized. Restore with a fresh socket; this replaces
any create-time socket, including when restoring an already-created container.
Dials resolve the restored root network namespace. Existing `Restore` methods
remain available when a fresh donation is unnecessary.

## Version 1 wire protocol

Each frame starts with a big-endian uint32 body length, excluding the four-byte
prefix. The body starts with version `1`, a one-byte kind, and a big-endian uint32
connection ID. This six-byte header is followed by:

| Kind | Value | Payload | Direction |
| --- | --- | --- | --- |
| OPEN | 1 | 16-byte IP address, big-endian uint16 port | Host to relay |
| DIAL_RESULT | 2 | One byte: 1 = connected, 2 = refused | Relay to host |
| DATA | 3 | Up to 64 KiB | Either |
| CLOSE_WRITE | 4 | Empty | Either |
| CLOSE | 5 | Empty | Either |

IPv4 addresses use IPv4-mapped IPv6 encoding; native IPv6 addresses are accepted.
The framing and golden vectors match Oca's version 1 host relay. A frame body is
limited to 65,560 bytes, with the separate 64 KiB DATA payload limit enforced.
Invalid versions, lengths, kinds, and duplicate live connection IDs terminate the
transport and all carried connections.

`CLOSE_WRITE` ends only the sender's data direction. All preceding DATA is written
before forwarding the EOF, and the opposite direction remains open for a
response. `CLOSE` cancels the whole connection, including a pending dial or queued
data. The relay emits a final CLOSE only after guest IO and the dial have ended.
The host may reuse that connection ID after receiving this acknowledgement.
Frames for unknown or closing connections are ignored.

There are at most 1,024 carried connections, counting pending dials. Excess OPENs
receive DIAL_RESULT(refused) followed by CLOSE without affecting existing
connections. Each connection has a queue of 16 frames. A full queue applies
backpressure to the entire transport, so consumers should finish or close stalled
connections. The netstack dial timeout is five seconds.

Closing the host transport or destroying the loader cancels pending dials and
joins guest IO. On Linux, a peer-closure probe also detects host teardown while
backpressure prevents the relay from reading. The host transport is a session:
half-closing that transport ends the session; use framed CLOSE_WRITE to half-close
an individual carried connection.
