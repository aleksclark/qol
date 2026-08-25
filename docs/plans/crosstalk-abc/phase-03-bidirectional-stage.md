# Phase 3: Bidirectional Qol Stage

## Goal

Implement a Qol-native `crosstalk` stage that runs inbound and outbound audio concurrently. Either direction can be enabled independently, and each port binds to a configured Qol channel and media profile, allowing the stage to attach to audio-producing or audio-consuming graph stages without changing Crosstalk authorization.

## BDD Success Criteria

### Scenario: Crosstalk sources a Qol graph

- **Given** a connected ABC with an authorized monitor track and a configured Qol output channel/profile
- **When** remote audio arrives
- **Then** the stage publishes ordered protobuf audio events on that exact channel
- **And** sequence numbers begin at zero, spans follow the RTP media clock, event IDs are unique for the connection epoch, and EOS is published on disconnect/reassignment.

### Scenario: Qol sinks audio into Crosstalk

- **Given** a configured Qol input channel and a connected ABC authorized to produce feed
- **When** any producer publishes valid PCM or encoded-stream events on that channel
- **Then** the stage converts and writes the audio to the ABC send track
- **And** uses a subscription mode that does not steal events from existing graph consumers by default
- **And** finalizes that Qol stream exactly once.

### Scenario: Full duplex operates independently

- **Given** both ports enabled
- **When** monitor audio and Qol producer audio flow simultaneously
- **Then** both directions progress without sharing sequence state, blocking one another, or feeding internal output directly back into input
- **And** direction-specific errors and counters identify the failing path.

### Scenario: Arbitrary channel attachment is validated

- **Given** operator-selected channels and profiles
- **When** the stage starts
- **Then** `StageSpec` exposes the selected input/output ports and accepted/produced semantic types
- **And** empty direction configuration disables that port
- **And** invalid self-loop/channel/profile combinations fail before connecting.

### Scenario: Connection epochs isolate Qol streams

- **Given** a disconnect or Crosstalk assignment change followed by reconnection
- **When** a new peer/welcome epoch begins
- **Then** the old epoch emits EOS and releases converter state
- **And** the new epoch gets a unique Qol session ID, fresh sequence zero, and non-colliding deterministic event IDs
- **And** no stale buffered audio crosses epochs.

## Implementation Instructions

1. Add a dedicated `Crosstalk` type implementing `qol.Stage`; do not force it through the existing single-input/single-output worker `Stage`. Its `Run` owns ABC transport, optional NATS subscription, converters, and concurrent lifecycle.
2. Define constructor/config fields for stage name, ABC endpoint/token reference, optional Qol sink-input channel, optional Crosstalk-source output channel, accepted/output profiles, subscription group mode, queue sizes/drop policy, and clock/ID collaborators for tests.
3. `Spec()` must contain zero or one Qol input and zero or one Qol output. Both absent is invalid. Ports include all semantic event types actually accepted/produced.
4. Outbound Qol→Crosstalk behavior:
   - subscribe live to the exact configured channel;
   - default `SubscribeOptions.Group` to empty so the bridge receives its own copy;
   - validate one active stream/epoch policy, contiguous sequence, duplicate handling, session identity, span, event type, and payload format;
   - enqueue bounded converter work and expose overflow/error counters.
5. Inbound Crosstalk→Qol behavior:
   - begin only after `Welcome`, assignment, and negotiated audio track are known;
   - derive a non-secret unique Qol session ID from assigned session + peer/epoch;
   - publish protobuf PCM or encoded-stream events with deterministic IDs, contiguous sequence, accurate spans, no false parents, and final event/EOS on epoch termination.
6. Define how unrelated Qol session IDs on the configured sink channel are handled. Initial safe policy should permit one active Qol stream at a time and reject or drop others with explicit metrics; do not mix sessions implicitly.
7. Prevent accidental digital loops by rejecting identical input/output channels and documenting that Crosstalk routing can still create an acoustic/graph loop if operators monitor the same feed they produce.
8. Aggregate goroutines with cancellation and first-error propagation. Closing the stage must unsubscribe, stop converters, close the ABC transport, and wait for all workers.
9. Add structured observability keyed by stage, direction, Qol session, Crosstalk assigned session, peer/epoch, channel, codec/profile, and terminal reason. Never include token/query values.
10. Keep the stage transport-neutral above `qol.Bus`; unit tests use the established in-memory bus, while later phases prove NATS.

## End-to-End Test Plan

- In Qol's in-memory graph tests, use a real local Pion peer and real converters while faking only the external Crosstalk server contract at the transport interface:
  - remote track → configurable PCM output channel;
  - remote track → encoded output accepted by STT;
  - TTS 22.05 kHz PCM input → local Opus receiver;
  - encoded capture input → local Opus receiver;
  - simultaneous full duplex;
  - source-only and sink-only modes.
- Assert event IDs, unique epoch session IDs, sequence zero/restart, span monotonicity, EOS once, profile metadata, no parents on external source events, duplicate input handling, queue overflow, and cancellation.
- Add a two-subscriber test proving the Crosstalk sink does not consume events away from record or another stage.
- Test reconnect with delayed old packets and queued input to ensure no cross-epoch leakage.
- Run:

```text
go test ./internal/worker ./internal/media
go test ./...
go vet ./...
```

## Anti-Cheating Audit

- Confirm the stage implements `qol.Stage` and uses the supplied `qol.Bus`; it must not import the concrete NATS adapter.
- Confirm tests drive `Run`, subscriptions, WebRTC tracks, and protobuf events rather than private handlers only.
- Verify source events originate from decoded remote media, not canned payloads emitted on connect.
- Verify sink tests decode media received from the WebRTC track, not just assert converter calls.
- Inspect subscription options to ensure default fan-out does not steal producer events.
- Check for unbounded goroutines/channels, swallowed handler errors, stale epoch buffers, reused event IDs, and missing EOS.
- Confirm self-loop validation is real and no hidden default channel couples the stage to only capture, STT, TTS, or record.

## Completion Gate

- [ ] Source-only, sink-only, and full-duplex modes pass.
- [ ] Configurable channels/profiles appear accurately in `StageSpec`.
- [ ] PCM and encoded producer/consumer attachments are proven.
- [ ] Sequence, spans, IDs, EOS, reconnect epochs, and session isolation pass.
- [ ] Default subscription preserves existing consumers.
- [ ] Overflow, cancellation, and direction-specific errors are observable and bounded.
- [ ] Full Qol unit, test, vet, and build gates pass.
