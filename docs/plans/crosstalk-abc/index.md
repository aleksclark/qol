# Crosstalk ABC Integration Plan

## Outcome

Completing this plan delivers a production-wired Qol `crosstalk` stage that authenticates as a Crosstalk Audio Booth Connector (ABC), receives the ABC's authorized monitor track into any configured Qol audio input channel, and publishes audio from any configured Qol producer channel back through the ABC's WebRTC source track. The stage converts between negotiated WebRTC Opus/RTP and explicit Qol PCM or encoded-stream profiles without requiring physical ALSA or PipeWire devices.

## Current-State Summary

- Crosstalk already provisions long-lived ABC tokens, authenticates `/ws/signaling?token=...` before peer allocation, assigns ABCs to sessions, derives their feed-production and monitor-listening permissions, and bridges real WebRTC audio (`crosstalk/server/api/webrtc.go:82-172`).
- The production `ct-abc` client creates a reliable control data channel, sends protobuf-v2 `Hello`, publishes one Opus track, receives a monitor track, and reconnects after disconnects (`crosstalk/cli/cmd/ct-abc/main.go:210-390`). Its media adapters are coupled to PipeWire/ALSA and ffmpeg (`crosstalk/cli/pion/audio.go:39-357`).
- Crosstalk currently accepts only Opus at 48 kHz with one or two channels; its mixer normalizes to mono 48 kHz and emits 20 ms Opus packets (`crosstalk/server/audiocodec/audiocodec.go:21-93`, `crosstalk/server/sessionrtc/bridge.go:720-742`). `Hello.capabilities` is informational and does not select the SDP codec.
- Crosstalk's reusable transport is not currently a stable external SDK. The CLI module depends on a sibling generated-protobuf module through a local `replace`, and `ct-abc` uses a hand-written v2 wire implementation to avoid cross-module coupling (`crosstalk/cli/go.mod:1-55`, `crosstalk/cli/protov2/protov2.go:1-17`).
- Qol already has transport-neutral `Stage`, multi-port `StageSpec`, `Bus`, ordered `Event`, media span, lineage, PCM payload, and encoded audio-stream contracts (`core.go:27-186`, `payloads.go:3-19`, `proto/qol/v1/qol.proto:14-42,77-82`).
- Qol's concrete generic worker runner assumes one input and one output, while a Crosstalk bridge must run receive and send paths concurrently. Existing graph channels and constructors are mostly fixed, but record already demonstrates configurable audio attachment (`internal/worker/graph.go:92-118`, `internal/worker/record.go:13-17`).
- Qol's NATS adapter offers live, non-durable fan-out or queue subscriptions but ignores handler failures and supplies no backpressure (`internal/eventbus/nats.go:81-107`). A real-time bridge therefore needs explicit bounded buffering, drop policy, and metrics.
- Qol has an ffmpeg decoder and subprocess lifecycle patterns, but no packetized WebRTC codec abstraction. STT currently accepts only encoded `AudioStreamPayload`; TTS emits 22.05 kHz mono S16LE PCM (`internal/media/ffmpeg.go:10-38`, `internal/worker/stt.go:10-24`, `internal/worker/tts.go:10-13`).
- Crosstalk has strong real Pion/PostgreSQL/Opus ABC integration tests, but no test launches an external Qol process as the ABC (`crosstalk/server/cmd/ct-server/abcflow_test.go:27-149`).

## Scope Boundaries

### In scope

- A reusable, versioned Crosstalk ABC transport package that owns signaling, protobuf-v2 control, SDP/ICE, reconnect, one local send track, and authorized remote audio tracks.
- A Qol-native bidirectional `crosstalk` stage with independently optional source and sink ports.
- Runtime configuration for Crosstalk URL/token, Qol source/output channels, input/output media profiles, queue behavior, reconnect policy, and ffmpeg path.
- Input support for Qol `qol.audio.stream`, `qol.audio.pcm.partial`, and `qol.audio.pcm.final` events where their declared media formats are valid.
- Output support for explicit PCM profiles and a streaming encoded profile usable by encoded-audio consumers such as the existing STT stage.
- Transcoding/resampling/channel conversion between Qol media and the actually negotiated WebRTC codec. Initial interoperable WebRTC codec is Opus/48 kHz/1-2 channels because that is Crosstalk's present server contract.
- Deterministic stream lifecycle, sequencing, spans, EOS, cancellation, bounded buffering, reconnect, logging, and metrics.
- Real cross-repository end-to-end proof in both directions with decoded audio-content assertions.

### Out of scope

- Replacing Crosstalk's mixer, ABC provisioning UI, assignment model, or authorization rules.
- Physical USB mixer control, ALSA/PipeWire capture/playback, or K2B hardware support in Qol.
- Video, data-channel application payloads beyond the ABC control protocol, or multiple simultaneous Crosstalk assignments per stage instance.
- Making NATS durable or changing all existing Qol stages to dynamic graph construction.
- Claiming arbitrary WebRTC codec support before Crosstalk offers and validates codecs other than Opus. The adapter architecture must be codec-neutral, but the first implementation must fail closed on unsupported negotiated codecs.
- Persisting ABC tokens in Qol's API store or exposing them to the browser.

## Global Constraints

- Crosstalk remains the source of truth for ABC authentication, session assignment, feed production, monitor selection, and negotiated WebRTC codec. Qol never broadens selectors or invents session permissions.
- The ABC token is accepted only through process configuration/secret injection, never logged, emitted in Qol events, exposed through telemetry, or embedded unredacted in errors.
- Crosstalk protocol types and signaling behavior have one owner. Qol must consume a released Crosstalk client module; it must not copy protobuf field numbers, generated files, or signaling logic into Qol.
- Qol owns graph semantics and conversion from WebRTC media to protobuf events. All NATS/persisted Qol representations remain protobuf-based.
- A configured sink subscription must use its own empty or unique group so it observes producer audio without stealing events from another graph consumer. Competing-consumer mode must require an explicit opt-in.
- Each successful ABC connection/assignment epoch maps to a unique Qol session ID derived from non-secret Crosstalk identifiers (for example assigned session plus peer/epoch). Reconnects must not reuse deterministic event IDs or overlap sequence spaces. Disconnect/reassignment emits EOS before a new epoch begins.
- WebRTC timestamps are the media clock. Source spans advance from RTP timestamp deltas, not wall-clock arrival. Sink pacing preserves Qol spans when available and otherwise uses codec frame duration.
- Bounded queues are mandatory on both directions. Overflow behavior is explicit and observable; control/signaling failure cannot silently produce unbounded memory growth or stale audio.
- Conversion runs as streaming processes/codecs, never buffers a whole Crosstalk session. Cancellation reaps subprocesses, closes WebRTC, and terminates subscriptions.
- Output format is explicit configuration, not guessed from the downstream stage name. Initial profiles should include `pcm-s16le` with rate/channels and `ogg-opus` for existing encoded-stream consumers.
- Qol and Crosstalk retain their existing package-layout rules. Any Crosstalk `spec/` correction is a separate spec-only commit with provenance.

## Phase Overview

| Phase | Goal | Depends on |
|---|---|---|
| [Phase 1: Reusable ABC transport contract](./phase-01-abc-transport.md) | Publish a device-independent Crosstalk ABC client package with real protocol conformance | None |
| [Phase 2: Streaming media conversion](./phase-02-media-conversion.md) | Convert Qol audio profiles to/from negotiated WebRTC Opus with bounded streaming behavior | Phase 1 |
| [Phase 3: Bidirectional Qol stage](./phase-03-bidirectional-stage.md) | Attach optional Crosstalk receive/send paths to arbitrary Qol audio channels | Phases 1-2 |
| [Phase 4: Runtime wiring and resilience](./phase-04-runtime-and-resilience.md) | Operate the stage securely through CLI, environment, containers, reconnects, and telemetry | Phase 3 |
| [Phase 5: Cross-system proof and rollout](./phase-05-e2e-and-rollout.md) | Prove real audio in both directions and finalize compatibility/spec documentation | Phases 1-4 |

## Requirement Traceability

| Requested capability | Primary phase | Final evidence |
|---|---|---|
| Connect Qol to Crosstalk as an ABC | Phases 1 and 4 | Actual `qol-worker --type crosstalk` authenticates with a provisioned token and receives `Welcome` from a real server |
| Source audio from Crosstalk | Phases 2 and 3 | Real monitor-track tone becomes ordered Qol audio events and is decoded by a downstream consumer |
| Sink audio to Crosstalk | Phases 2 and 3 | Qol producer tone reaches the ABC WebRTC source track and is decoded by a real Crosstalk listener |
| Connect to any stage needing/producing audio | Phase 3 | Independent configurable input/output channels and media profiles are tested against PCM and encoded-stream consumers/producers |
| Re-encode between negotiated WebRTC and internal formats | Phase 2 | Bidirectional content/frequency/duration tests across Opus/48 kHz and multiple Qol PCM/encoded profiles |
| Real operational reliability | Phases 4 and 5 | Reconnect, reassignment, invalid token, unsupported codec, overflow, cancellation, and restart scenarios through production wiring |

## Completion Rule

The plan is complete only when all five phase gates pass and an actual Qol worker process connects to a real PostgreSQL-backed Crosstalk server using a provisioned ABC token. A real monitor track must traverse WebRTC, conversion, protobuf Qol events, NATS, and a downstream audio consumer; independently, real Qol audio events must traverse NATS, conversion, WebRTC, Crosstalk's mixer, and a real listener. Both directions must assert decoded content, timing, EOS, and session isolation. Unit-only Pion peers, copied protocol definitions, direct mixer injection, mocked first-party transports, status-only assertions, or tests that bypass NATS/WebSocket/WebRTC/Opus do not satisfy completion.
