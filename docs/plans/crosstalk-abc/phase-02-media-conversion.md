# Phase 2: Streaming Media Conversion

## Goal

Add a Qol-side streaming media boundary that converts declared Qol audio formats to the codec negotiated by the ABC WebRTC transport and converts remote WebRTC audio to an explicitly configured Qol output profile. This phase proves codec correctness independently of NATS and graph lifecycle.

## BDD Success Criteria

### Scenario: PCM producer reaches negotiated WebRTC Opus

- **Given** Qol S16LE or F32LE PCM with a declared rate/channel count and an ABC connection negotiated as Opus/48 kHz
- **When** frames are written to the outbound converter
- **Then** it resamples, mixes/splits channels as configured, paces 20 ms frames, and emits valid RTP Opus
- **And** a real Opus decoder recovers the expected tone, duration, and non-silent energy.

### Scenario: Encoded Qol stream reaches WebRTC

- **Given** a supported streaming encoded input such as Ogg/Opus or browser capture media
- **When** chunks arrive incrementally with EOS
- **Then** conversion begins before EOS, does not buffer the complete stream, and emits negotiated WebRTC media
- **And** malformed or unsupported media fails with a format-specific error.

### Scenario: WebRTC audio becomes configured Qol PCM

- **Given** remote Opus RTP with negotiated clock rate/channels
- **When** it enters the inbound converter
- **Then** output chunks carry the configured Qol PCM rate, channels, and sample format
- **And** packet timestamps produce monotonic media spans
- **And** loss/reordering policy is observable rather than silently corrupting timing.

### Scenario: WebRTC audio becomes a streaming encoded profile

- **Given** remote Opus RTP and `ogg-opus` output configuration
- **When** audio starts and later ends
- **Then** valid Ogg/Opus chunks are available before EOS
- **And** the existing STT ffmpeg input can decode the stream incrementally
- **And** finalization emits exactly one EOS.

### Scenario: Unsupported negotiated codec fails closed

- **Given** a codec outside the registered converter matrix
- **When** the WebRTC transport reports negotiation complete
- **Then** the converter refuses media startup with the codec parameters in a non-secret error
- **And** does not relabel unconverted bytes as another format.

## Implementation Instructions

1. Add Qol media contracts for negotiated codec descriptors, input/output profiles, timestamped frames, and streaming converter lifecycle. Keep these independent of Pion and ffmpeg so tests can exercise policy separately from adapters.
2. Implement Pion-facing RTP adapters in a dependency-specific package and conversion adapters around ffmpeg or a justified Opus library. Initial WebRTC matrix is Opus/48 kHz/1-2 channels; normalize Crosstalk stereo to the configured Qol profile.
3. Support outbound Qol payloads:
   - `TypeAudioPCM`/`TypeAudioPCMFinal`: validate protobuf format on every chunk and reject mid-stream format changes unless an explicit converter restart is defined;
   - `TypeAudioStream`: honor `media_type`, stream bytes incrementally, and finalize on `end_of_stream`.
4. Support inbound Qol profiles:
   - PCM S16LE initially, with explicit rate/channels and optional F32LE if implemented and tested;
   - streaming Ogg/Opus for encoded-audio consumers such as current STT.
5. Define frame/time rules: use RTP clock/timestamps for inbound spans; outbound packet timestamps advance from encoded sample counts and Qol spans. Discontinuities, late packets, and converter resets increment structured counters.
6. Implement bounded input/output queues and specify overflow policy. For real-time media, drop complete oldest frames or fail the epoch according to configuration; never block Pion callbacks indefinitely or split sample frames arbitrarily.
7. Ensure process lifecycle closes stdin, drains stdout, waits/reaps children, and propagates first terminal error. No fixed sleeps for readiness; use process/packet signals and deadlines.
8. Add converter metrics/log fields: direction, source profile, negotiated codec, output profile, frame counts, dropped frames, conversion latency, process exits, and EOS reason.
9. Do not modify STT merely to hide invalid framing. The encoded output must be a genuinely decodable streaming container, or a separately planned STT PCM input extension must be explicit and tested.

## End-to-End Test Plan

- Run real ffmpeg/Pion/Opus loop tests with deterministic tones for:
  - 22.05 kHz mono S16LE (current TTS) to Opus/48 kHz;
  - 16 kHz mono S16LE to Opus/48 kHz;
  - 48 kHz stereo F32LE, if supported, to Crosstalk mono behavior;
  - Ogg/Opus input to RTP Opus;
  - RTP Opus to 16 kHz mono S16LE;
  - RTP Opus to streaming Ogg/Opus consumed by the existing STT ffmpeg command.
- Assert frequency, duration tolerance, channel behavior, first-output latency before EOS, RTP timestamp increments, finalization, and absence of orphan ffmpeg processes.
- Inject malformed headers, mid-stream PCM format changes, packet loss/reordering, queue overflow, unsupported codec, converter crash, and cancellation.
- Run Qol commands:

```text
go test ./internal/media ./internal/worker
go test ./...
go vet ./...
go build ./cmd/...
```

## Anti-Cheating Audit

- Inspect that tests decode emitted Opus/Ogg and analyze PCM content rather than comparing fixture bytes or checking packet counts only.
- Confirm production code does not special-case test tones, filenames, or environment flags.
- Confirm encoded output is a valid streaming container, not raw RTP or concatenated Opus mislabeled as Ogg.
- Verify no whole-session `io.ReadAll`, unbounded channel, or hidden disk spool substitutes for streaming.
- Confirm negotiated codec parameters drive converter selection and unsupported codecs are rejected.
- Force subprocess failures and inspect process tables to prove cleanup.
- Verify format metadata matches actual decoded rate/channels/sample representation.

## Completion Gate

- [ ] Every declared media profile round-trips through a real decoder with content/timing assertions.
- [ ] Conversion starts before EOS and stays bounded.
- [ ] Timestamp, loss, overflow, EOS, cancellation, and process-failure behavior is tested.
- [ ] Unsupported codecs and malformed/mutating formats fail closed.
- [ ] No child process or temporary resource leaks.
- [ ] Focused and full Qol Go gates pass.
