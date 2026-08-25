# Phase 1: Reusable ABC Transport Contract

## Goal

Extract the proven Crosstalk ABC signaling/control/WebRTC lifecycle into a device-independent, externally consumable client package. This removes ALSA/PipeWire assumptions and prevents Qol from duplicating a private protocol. Media conversion and Qol graph behavior remain outside this phase.

## BDD Success Criteria

### Scenario: External client authenticates as an assigned ABC

- **Given** a real Crosstalk server, a provisioned ABC token, and a session assignment
- **When** a client built only against the released ABC transport package connects
- **Then** authentication occurs before peer allocation
- **And** the control channel exchanges protobuf-v2 `Hello` and `Welcome`
- **And** the client reports the assigned session and negotiated audio codec.

### Scenario: Transport exposes both media directions without devices

- **Given** an assigned ABC authorized to produce feed and listen to one monitor channel
- **When** it connects with one local audio track
- **Then** the package exposes a packet/frame writer for the negotiated send track
- **And** exposes authorized remote audio tracks through callbacks/readers
- **And** never opens PipeWire, ALSA, or ffmpeg itself.

### Scenario: Authentication and protocol failures fail closed

- **Given** an invalid/deleted token, malformed control frame, or incompatible protocol message
- **When** the client connects or handles control data
- **Then** it returns a typed non-retryable authentication/protocol error where applicable
- **And** does not leak the token in logs or error text
- **And** closes any partially allocated peer resources.

### Scenario: Assignment change is observable

- **Given** a connected ABC whose assignment or monitor channel changes
- **When** Crosstalk closes/restarts the peer under its current lifecycle
- **Then** the client reports a terminal connection epoch and reason
- **And** a caller can reconnect with bounded backoff
- **And** the new `Welcome` is surfaced as a distinct epoch.

## Implementation Instructions

1. In Crosstalk, define dependency-free ABC transport contracts for connection state, welcome/assignment, negotiated codec, outgoing RTP/encoded frames, incoming tracks, and typed errors. Keep hardware mixer commands optional and separate from the media lifecycle.
2. Refactor `cli/pion/connection.go` so a reusable package owns `/ws/signaling`, ICE, SDP offer/answer/renegotiation, the reliable `control` channel, RTCP draining, track callbacks, and clean shutdown. Keep `cli/pion/audio.go` as a device adapter layered above it.
3. Move v2 control ownership to generated protobuf or one shared validated codec. Do not introduce another hand-written copy. Resolve the current local `replace github.com/aleksclark/crosstalk/proto/gen/go => ../proto/gen/go` so an external module can consume tagged/pseudo-versioned Crosstalk modules from a clean checkout.
4. Preserve current production behavior: client creates the control channel and a pre-offer sendrecv Opus transceiver; server sends `Welcome`; server-side assignment/monitor updates may close the connection.
5. Expose the actual `RTPCodecParameters` selected by SDP. Advertised `Hello.capabilities` must accurately describe what the client offers, but this phase must not claim those fields drive server codec selection.
6. Add token redaction at the URL/log boundary. Structured logs may include server host, peer ID, assigned session, codec, and state, never query credentials.
7. Keep `ct-abc` behavior unchanged by rebuilding it on the reusable package and its existing hardware capture/playback adapters.
8. Add module release/workspace instructions so Crosstalk's own development remains local while Qol can pin a real module version or commit without repository-relative replacements.

## End-to-End Test Plan

- Extend Crosstalk's real integration harness to create a PostgreSQL-backed ABC, assign it, connect through the reusable package, receive `Welcome`, and verify the server's connected/source state.
- Send a generated Opus tone through the package's local track and assert a real listener decodes it, reusing the strength of `server/cmd/ct-server/abcflow_test.go` without its private `abcClient` signaling implementation.
- Configure a monitor channel and assert the package receives a real remote track and decodes a known server tone through a test-only codec reader above the transport.
- Delete the ABC, retry the token, send malformed control data, and force assignment change; assert typed failures, cleanup, and no token leakage.
- Run from `/home/aleks/work/projects/crosstalk`:

```text
task test:unit:go
task test:integration
task lint:go
```

## Anti-Cheating Audit

- Confirm the integration test imports the public reusable package rather than copying `newABCClient` or directly constructing server peers.
- Confirm the package dials the real `/ws/signaling` endpoint and uses the generated v2 protobuf contract.
- Search Qol and Crosstalk for duplicated control field numbers or copied signaling structs.
- Verify no audio device process is started by the transport package.
- Verify tests assert decoded audio and server state, not only `Welcome` or ICE-connected status.
- Inspect logs and errors with a sentinel token to prove redaction.
- Confirm the external-consumer test runs outside Crosstalk's implicit local `replace` graph.

## Completion Gate

- [ ] Reusable ABC transport API is device-independent and documented.
- [ ] `ct-abc` uses it without behavior regression.
- [ ] External-module consumption works from a clean checkout.
- [ ] Real bidirectional media and lifecycle tests pass.
- [ ] Invalid auth/protocol paths are typed, closed, and redacted.
- [ ] Crosstalk unit, integration, and Go lint gates pass.
