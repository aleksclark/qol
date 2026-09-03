# Phase 1: Sequential Sink Sessions

## Goal

Keep the Qol-to-Crosstalk sink usable when multiple Qol sessions arrive sequentially during one assigned ABC epoch. The current event handler clears `activeSession` at EOS, but the outbound converter and forwarding goroutine terminate, so later accepted sessions have no consumer. This phase establishes a repeatable per-stream lifecycle before broader failure handling changes.

## BDD Success Criteria

### Scenario: A second session follows EOS

- **Given** one assigned Crosstalk epoch with a configured Qol sink channel
- **When** session A sends contiguous audio ending in EOS and session B then starts at sequence zero
- **Then** both sessions produce decodable RTP audio on the same ABC send track
- **And** session B is not queued into a terminated converter
- **And** each session emits exactly one media termination boundary.

### Scenario: Sessions cannot overlap

- **Given** session A is active and has not sent EOS
- **When** an event from session B arrives
- **Then** session B is rejected according to the existing isolation policy
- **And** session A continues without converter restart or mixed audio
- **And** rejection metrics identify the discarded session.

### Scenario: Restart failure is visible

- **Given** session A has completed and the next per-session converter cannot start
- **When** session B begins
- **Then** session B is not reported as successfully accepted
- **And** the epoch leaves readiness or restarts according to the phase-2 failure contract
- **And** no stale session-A audio is replayed.

## Implementation Instructions

1. Rework ownership across `internal/worker/crosstalk.go` and `internal/media/outbound.go` so EOS ends one Qol stream without leaving a dead converter installed. Choose an explicit lifecycle: either keep a reusable converter that can delimit streams safely, or create and atomically install a fresh converter/forwarder per accepted session.
2. Preserve the single active sink-session rule, contiguous sequence validation, duplicate handling, bounded queue behavior, and one ABC send track per epoch.
3. Ensure state changes occur only after the new converter can consume events. Synchronize `activeSession`, `nextSeq`, converter replacement, and epoch cancellation so concurrent NATS callbacks cannot write to a closing instance.
4. Drain or discard only the completed stream. Never carry queued events, format state, FFmpeg state, RTP timestamps, or errors into the next session.
5. Add focused tests to `internal/worker/crosstalk_test.go` and converter tests where needed. Exercise two sequential PCM sessions and two sequential encoded sessions, plus an overlapping-session rejection case.
6. Run after each logical change:

```text
go test ./internal/media ./internal/worker
go test -race ./internal/media ./internal/worker
```

## End-to-End Test Plan

- Start real NATS and a real Crosstalk server, then launch the compiled `qol-worker run --type crosstalk` in sink-only mode.
- Publish a deterministic tone as session A through the configured NATS channel, send EOS, then publish a different tone as session B starting at sequence zero without reconnecting the ABC.
- Decode audio received by a real Crosstalk listener and assert both frequencies, ordering, duration, and absence of cross-session samples.
- Publish session B before session A EOS in a separate run and assert B is rejected while A remains audible.
- Use `QOL_CROSSTALK_E2E=1 task test:e2e:crosstalk-qol` after extending the compiled proof for sequential sessions.

## Anti-Cheating Audit

- Confirm the regression test uses one uninterrupted ABC epoch; reconnecting between sessions hides the defect.
- Confirm audio from both sessions traverses NATS, production conversion, WebRTC, and a real decoder.
- Inspect that tests assert decoded content, not only frame counts or health status.
- Check that no test-only converter reset or direct `forwardOutbound` invocation bypasses the stage boundary.
- Verify queues and goroutines from session A terminate before or independently from session B without leaks.

## Completion Gate

- [ ] Two sequential PCM sessions work in one ABC epoch.
- [ ] Two sequential encoded sessions work in one ABC epoch.
- [ ] Overlapping sessions remain isolated.
- [ ] Converter replacement is race-safe and bounded.
- [ ] Decoded end-to-end audio proves both sessions.
- [ ] Focused tests and race tests pass.
