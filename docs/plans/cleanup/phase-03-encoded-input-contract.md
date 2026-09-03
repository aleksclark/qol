# Phase 3: Encoded Input Contract

## Goal

Make the outbound converter honor the declared `media_type` of every accepted `TypeAudioStream`. The current code accepts MP3, WebM, and raw Opus but collapses them to the Ogg profile and forces FFmpeg's Ogg demuxer. This phase either implements correct streaming demuxing for each advertised type or removes unsupported types from validation and documentation.

## BDD Success Criteria

### Scenario: Every advertised media type converts

- **Given** a valid incrementally delivered stream for any media type accepted by the production validator
- **When** it is written through the Qol sink path
- **Then** the matching demuxer consumes it without whole-stream buffering
- **And** valid Opus RTP is emitted before or at the format's minimum streaming startup point
- **And** decoded audio matches expected content and duration.

### Scenario: Unsupported type fails synchronously

- **Given** a media type with no implemented and tested demuxing path
- **When** the first event or direct stream write is validated
- **Then** it returns `ErrUnsupportedMedia` before accepting bytes into the queue
- **And** health does not remain ready after the stage receives a terminal unsupported stream.

### Scenario: Media type cannot change mid-stream

- **Given** an active encoded stream
- **When** a later chunk declares a different media type
- **Then** the stream fails with a format-change error
- **And** bytes from different containers are never concatenated into one FFmpeg input.

## Implementation Instructions

1. Preserve normalized media type in `outboundItem` rather than representing every encoded stream as `ProfileOggOpus`.
2. Define one authoritative supported-input registry in `internal/media` mapping exact media types/aliases to validation and FFmpeg demuxer arguments.
3. Decide support from proven streaming behavior. At minimum retain Ogg/Opus. Add MP3 or WebM only if chunked production conversion and decoded-content tests pass. Treat raw `audio/opus` carefully because packet framing cannot be inferred from concatenated payload bytes.
4. Update `WriteStream`, `decodeOutbound`, format-change checks, and `outboundArgs` to use the declared container.
5. Reject empty, malformed, unsupported, or mid-stream-mutating declarations with stable errors that the stage propagates under phase 2.
6. Align README/help text and tests with the implemented matrix; do not retain aspirational formats in code or docs.
7. Add table-driven converter and worker tests covering every accepted type and representative rejected aliases/malformed streams.
8. Run:

```text
go test ./internal/media ./internal/worker
go test -race ./internal/media ./internal/worker
```

## End-to-End Test Plan

- For every supported media type, generate a deterministic tone using a real encoder, split it across irregular chunk boundaries, publish it through real NATS to a compiled Crosstalk worker, and decode the WebRTC output at a real listener.
- Assert frequency, duration, first-output behavior, one EOS, and no ready-but-silent state.
- Publish an unsupported type and a mid-stream media-type change; assert deterministic failure, epoch/health transition, and no mislabeled RTP output.
- Run the extended proof with `QOL_CROSSTALK_E2E=1 task test:e2e:crosstalk-qol`.

## Anti-Cheating Audit

- Compare the accepted registry directly with test cases and documentation; every accepted entry needs decoded-content evidence.
- Inspect FFmpeg arguments to ensure the selected demuxer comes from the media type, not a hard-coded Ogg path.
- Confirm tests use irregular chunking and do not pass an entire file as one convenient buffer.
- Confirm raw bytes are not relabeled as Ogg or Opus without valid framing.
- Verify malformed input reaches production validation rather than a test-only parser.

## Completion Gate

- [ ] Every accepted media type selects the correct demuxer.
- [ ] Every accepted media type has real decoded-content coverage.
- [ ] Unsupported and malformed types fail before queue acceptance.
- [ ] Mid-stream type changes fail without mixed output.
- [ ] Documentation and CLI behavior match the tested support matrix.
- [ ] Focused, race, and live end-to-end tests pass.
