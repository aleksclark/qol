# Phase 5: Encoded Media Spans

## Goal

Derive Ogg/Opus output spans from media timing rather than assigning 20 ms to each arbitrary FFmpeg stdout read. Pipe reads may contain headers, partial pages, or multiple packets, so the current spans vary with buffering and can misstate total duration.

## BDD Success Criteria

### Scenario: Encoded duration matches media

- **Given** remote RTP Opus with known timestamps and duration
- **When** it is converted to streaming Ogg/Opus chunks
- **Then** emitted spans are monotonic and contiguous under normal input
- **And** the final span duration matches decoded media duration within an explicit codec/container tolerance
- **And** results do not depend on stdout read sizes.

### Scenario: Headers do not advance time

- **Given** Ogg headers emitted before audio pages
- **When** the converter publishes encoded chunks
- **Then** header-only bytes do not invent an extra 20 ms of media
- **And** consumers still receive a valid incrementally decodable Ogg stream.

### Scenario: RTP discontinuity is observable

- **Given** loss, reordering, or a timestamp jump in remote RTP
- **When** encoded output is produced
- **Then** span behavior follows a documented discontinuity policy based on the media clock
- **And** discontinuity metrics remain observable
- **And** spans never move backward.

## Implementation Instructions

1. Define the timing source explicitly. Prefer RTP timestamps already carried by `media.Frame`; if spans must be assigned after FFmpeg packaging, parse Ogg page granule positions sufficiently to associate byte ranges with media progress.
2. Remove the fixed 20 ms-per-read rule from `internal/media/inbound.go`. Read boundaries may determine chunk payload sizes but must not determine media duration.
3. Preserve streaming output and valid Ogg framing. Do not buffer the complete session merely to compute final duration.
4. Establish semantics for header chunks, pages containing multiple packets, final granule trimming, RTP wraparound, loss, and reordered packets.
5. Add deterministic tests that force different reader chunk sizes for identical encoded bytes and assert equivalent span timelines and final duration.
6. Extend `internal/media/convert_test.go` beyond monotonicity: decode output, calculate sample duration, and compare it with the final span and RTP timestamp progression.
7. Verify downstream event construction in `internal/worker/crosstalk.go` preserves corrected spans and EOS cursor values.
8. Run:

```text
go test ./internal/media ./internal/worker
go test -race ./internal/media ./internal/worker
```

## End-to-End Test Plan

- Send a deterministic, precisely timed Opus source through a real Crosstalk monitor track into a compiled Qol worker configured for `ogg-opus` output.
- Subscribe through real NATS, concatenate protobuf stream payloads, decode them with FFmpeg, and compare decoded sample duration with event spans and source RTP duration.
- Repeat with different packet batching and process scheduling conditions; timeline results must remain within the same explicit tolerance.
- Add packet loss and timestamp wrap/discontinuity cases where the live harness permits them and assert monotonic spans plus metrics.
- Run `QOL_CROSSTALK_E2E=1 task test:e2e:crosstalk-qol`.

## Anti-Cheating Audit

- Confirm timing assertions compare decoded samples and RTP timestamps, not only monotonic counters.
- Search for fixed span increments tied to `Read` calls or byte-buffer sizes.
- Verify tests vary reader chunking for identical media and obtain equivalent timing.
- Confirm no whole-session buffering or post-hoc fixture metadata substitutes for streaming timing.
- Inspect final Ogg granule handling so codec pre-skip/padding is not silently counted as content.

## Completion Gate

- [ ] Header-only output does not advance media time.
- [ ] Span duration matches decoded duration within documented tolerance.
- [ ] Timing is invariant to stdout read boundaries.
- [ ] Loss, reordering, wraparound, and EOS policies are documented and tested.
- [ ] Streaming behavior remains bounded and incrementally decodable.
- [ ] Focused, race, and live end-to-end tests pass.
