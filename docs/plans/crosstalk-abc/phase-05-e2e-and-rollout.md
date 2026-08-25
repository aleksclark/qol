# Phase 5: Cross-System Proof and Rollout

## Goal

Prove the complete Qol↔Crosstalk integration through production boundaries in both directions, establish cross-repository compatibility gates, and update authoritative documentation/specs only after behavior is demonstrated.

## BDD Success Criteria

### Scenario: Crosstalk floor audio enters the Qol graph

- **Given** a real PostgreSQL-backed Crosstalk server, assigned Qol ABC, monitor channel carrying a known tone/speech fixture, real NATS, and a Qol source output profile
- **When** the actual Qol worker connects
- **Then** a downstream Qol consumer receives protobuf audio events before the source ends
- **And** decoded content, duration, rate/channels, sequence, spans, and EOS match the source within stated tolerances.

### Scenario: Qol-produced audio reaches a Crosstalk listener

- **Given** a Qol producer channel carrying current TTS 22.05 kHz mono S16LE or encoded capture audio
- **When** the actual Qol worker consumes that channel
- **Then** a real Crosstalk listener receives it through the ABC feed/mixer path
- **And** decoded frequency/speech content and duration prove transcoding rather than status-only success.

### Scenario: Full translation loop runs live

- **Given** Crosstalk floor audio routed to Qol STT→translate→TTS and the Qol TTS channel routed back through the ABC
- **When** a finite English fixture is played
- **Then** Qol begins publishing translated Spanish audio back to Crosstalk before the floor source ends when model latency permits
- **And** a real listener receives non-silent Spanish output
- **And** both systems expose correlated, non-secret session/peer/epoch identifiers.

### Scenario: Isolation and failure boundaries hold

- **Given** two Crosstalk sessions/ABCs and concurrent Qol epochs
- **When** audio, reassignment, disconnect, token revocation, or unsupported codec affects one
- **Then** the other session continues without receiving foreign audio
- **And** no event IDs, buffered frames, or media cross session/epoch boundaries
- **And** operators can diagnose the failed side from structured evidence.

### Scenario: Compatibility changes fail visibly

- **Given** supported Crosstalk ABC module/protocol and Qol media profile versions
- **When** either repository changes signaling, control protobuf, codec policy, or payload format incompatibly
- **Then** CI contract tests fail before release
- **And** the compatibility matrix and upgrade instructions identify the required coordinated change.

## Implementation Instructions

1. Add a cross-repository harness, owned in the repository best able to launch both artifacts, that builds a released/pinned Crosstalk server and Qol worker, starts PostgreSQL and NATS, provisions sessions/channels/ABC through public APIs, and injects secrets at runtime.
2. Reuse real Pion/browser listeners and decoded-tone analysis from Crosstalk's `abcflow_test.go`, `audioflow_test.go`, and Playwright golden suite. Do not call `sessionrtc` internals or publish directly into its mixer.
3. Exercise both Qol output profiles: PCM for a simple downstream consumer/record path and streaming Ogg/Opus through the current STT process. Exercise both Qol input forms: current TTS PCM and an encoded audio stream.
4. Add a finite-session harness that can terminate monitor input cleanly and assert EOS despite Crosstalk's normally continuous ABC connection. For continuous operation, define epoch termination via disconnect/reassignment/shutdown rather than inventing silence-based EOS.
5. Record latency budgets: WebRTC receive to first Qol event, Qol event to first Crosstalk decoded frame, end-to-end translation first audio, and reconnect recovery. Set generous deterministic CI thresholds and report measured values.
6. Add concurrency/isolation coverage with two assigned ABCs and distinguishable tones. Verify session IDs, event IDs, NATS channels, and Crosstalk listener content.
7. Add contract tests pinned to the released Crosstalk ABC client module and generated control schema. Establish release ordering: Crosstalk transport release first, then Qol dependency update; incompatible protocol changes require a new supported version/matrix entry.
8. Update Qol documentation with complete deployment and graph examples. In Crosstalk, add an external ABC integration document and correct the relevant `spec/` sections/confidence scores in separate spec-only commits after tests pass.
9. Add CI tasks without making Qol's ordinary unit suite depend on the sibling checkout. Unit/contract tests run in each repo; the cross-system suite runs in an explicit integration workflow with pinned revisions and retains redacted logs/audio artifacts on failure.
10. Perform security review of long-lived token storage, URL redaction, TLS verification, container network exposure, dependency provenance, and artifact retention.

## End-to-End Test Plan

The final suite must include:

1. **ABC admission:** provision, assign, connect actual Qol worker, receive `Welcome`, verify server connected/source state.
2. **Crosstalk→Qol PCM:** real monitor tone → WebRTC Opus → configured Qol PCM → NATS consumer; assert content/timing/EOS.
3. **Crosstalk→Qol encoded/STT:** monitor speech/tone → streaming Ogg/Opus Qol events → actual STT ffmpeg/model boundary; assert early decode/text or deterministic decoder evidence.
4. **Qol PCM→Crosstalk:** current TTS-format PCM → NATS → Qol stage → WebRTC → Crosstalk mixer → real listener; assert decoded content.
5. **Qol encoded→Crosstalk:** encoded capture fixture through the same public path.
6. **Full duplex:** simultaneous distinguishable inbound/outbound tones, proving independent directions and no local self-loop.
7. **Translation graph:** finite English speech through real Qol stages, Spanish audio received before/around source completion with measured latency.
8. **Isolation:** two ABCs/sessions with distinct tones and concurrent events; zero cross-audio.
9. **Lifecycle:** unassigned, late assignment, monitor change, server restart, NATS restart, token deletion, unsupported codec, malformed internal format, queue pressure, and SIGTERM.
10. **Compatibility:** build Qol from a clean checkout using only pinned Crosstalk module versions, not sibling replacements.

Required commands at completion:

```text
# Qol
go test ./...
go vet ./...
go build ./cmd/...
npm --prefix web test -- --run

# Crosstalk
task test:unit:go
task test:integration
task test:e2e
task lint:go

# New explicit cross-system task
# Name must be updated here to the actual committed task, e.g.:
task test:e2e:crosstalk-qol
```

## Anti-Cheating Audit

- Confirm the suite launches compiled Qol and Crosstalk processes, PostgreSQL, NATS, real WebSocket signaling, Pion WebRTC, and real Opus conversion.
- Confirm ABC/session/channel/token setup uses public REST APIs and the worker uses the public reusable ABC package.
- Verify audio assertions decode what listeners/consumers actually receive and check frequency/content, duration, and timing.
- Reject direct NATS fixture publication for paths claimed to prove a real upstream Qol stage unless that specific test is only an adapter test.
- Reject direct Crosstalk mixer/sessionrtc calls, mocked first-party servers, copied signaling clients, and status-only assertions.
- Inspect two-session tests for unique tones and both positive and negative assertions.
- Verify failure artifacts redact tokens/JWTs and do not retain sensitive speech by default.
- Confirm no sibling `replace` directive or uncommitted local module is needed in CI.
- Ensure spec confidence changes cite named passing tests and remain separate commits.
- Review skipped tests, broad retries, arbitrary sleeps, disabled TLS/auth, and test-only production branches.

## Completion Gate

- [ ] Actual binaries pass real bidirectional and full-duplex audio-content tests.
- [ ] PCM and encoded profiles work in both required attachment roles.
- [ ] Full live translation path is proven with latency evidence.
- [ ] Isolation, lifecycle, security, and compatibility scenarios pass.
- [ ] Both repositories' required unit/integration/lint/build suites pass.
- [ ] Explicit cross-system CI runs with pinned revisions and redacted artifacts.
- [ ] User documentation and compatibility matrix match tested behavior.
- [ ] Crosstalk spec corrections/confidence updates are evidence-backed and separately committed.
