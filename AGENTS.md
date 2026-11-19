# Qol agent guide

Qol is a streaming voice translation system with a Go control plane and stage workers, a React administrator UI, and core NATS for event transport. Persistence is a local disk store. Keep all wire and persisted representations protobuf-based.

## Commands

- Focused Go package: `go test ./internal/<package>`
- All Go tests: `task test` or `go test ./...`
- Web tests: `task web-test` or `npm --prefix web test -- --run`
- Web typecheck: `npm --prefix web run typecheck`
- Protobuf lint/generation: `buf lint && buf generate` (Go plus Python processor bindings in `e2e/processors/gen`)
- Development stack: run `task dev`. Dedicated worker images contain their processor dependencies and models.
- Stop development: `task dev:down`; API data persists. Recorded Ogg/Opus files land in gitignored `tmp/`.
- Real-model graph test (host processors, no NATS): see `docs/speech-to-speech-roadmap.md`; it is skipped unless `QOL_RUN_REAL_GRAPH_E2E=1` and reports source-to-output prosody carry-through.

Full local gate:

```sh
buf lint
buf generate
go test ./...
go vet ./...
go build ./cmd/...
npm --prefix web ci
npm --prefix web run typecheck
npm --prefix web run build
docker compose -f docker-compose.e2e.yml up --build --abort-on-container-exit --exit-code-from e2e
docker compose -f docker-compose.e2e.yml down
```

The Compose e2e service is startup-only. It does not run model inference or the upload flow. Tool versions are Go `1.26.6`, Task `3.52.0`, and Node `24.15.0` in Docker.

## Architecture and data flow

- `cmd/qol-api`: Cobra entrypoint, NATS wiring, disk persistence, HTTP/HTTP3 lifecycle, and admin bootstrap.
- `cmd/qol-worker`: composition root for one stage selected by `--type=capture|stt-whisper|prosody|translate|tts|record`. `--name`, `--input`, and `--output` distinguish record instances.
- `cmd/qol-feed`: CLI that logs in, takes an upload ticket, and streams a file through WebTransport at playback rate like a live microphone.
- Root package (`core.go`, `payloads.go`, `types.go`): transport-independent generic event, bus, stage, port, replay, and semantic payload contracts.
- `internal/api`: protobuf HTTP auth and WebTransport upload. It publishes uploaded chunks to the capture subject and waits for both record completion events.
- `internal/worker`: `qol.Stage` implementations, persistent subprocess adapters behind the generic bus boundary, and atomic Ogg/Opus storage.
- `internal/prosody`: pure-Go source delivery analysis (YIN pitch, level, syllable rate, pauses) and per-session relative baselines.
- `internal/eventbus`: core NATS publish/subscribe, queue groups, and protobuf serialization.
- `internal/wire`: the only domain/protobuf conversion layer plus varint-delimited framing.
- `internal/auth` and `internal/store`: users, sessions, one-use upload tickets, and memory/disk adapters.
- `proto/qol/v1`: source wire schema. Generated Go is in `gen/qol/v1`; generated browser code is in `web/src/gen/qol/v1`.

Graph subjects are fixed:

1. `qol.graph.capture.input`: source audio chunks from the API.
2. `qol.graph.capture.output`: canonical capture fan-out copy.
3. `qol.graph.stt-whisper.input`: source audio for STT.
4. `qol.graph.record-en.input`: source audio for the English record instance.
5. `qol.graph.prosody.audio`: source audio for prosody analysis.
6. `qol.graph.prosody.input`: English utterances with source-media spans from STT.
7. `qol.graph.translate.input`: English utterances annotated with `ProsodyPayload`.
8. `qol.graph.tts.input`: Spanish utterances carrying the source `ProsodyPayload`.
9. `qol.graph.record-es.input`: Spanish S16LE PCM chunks.
10. `qol.graph.record-en.completed`: English Ogg/Opus metadata.
11. `qol.graph.record-es.completed`: Spanish Ogg/Opus metadata.

Capture publishes each incoming audio-stream event onto its four output ports. STT, prosody, and `record-en` subscribe to their own input subjects. Prosody joins each STT utterance with the decoded source PCM covering its span; translate and TTS keep the utterance span and prosody on every output.

Every externally visible worker implements `qol.Stage` through `Spec()` and `Run(ctx, bus)`. Stage ports declare channels and accepted/produced semantic event types. Processors are long-lived subprocesses started once per stream (STT) or once per worker (translate, TTS) so models load once.

## Invariants and gotchas

- Processor executables read stdin and write stdout. STT reads the source media stream and writes varint-delimited `RecognizedUtterance` frames; translation reads one English line and writes one Spanish line; TTS reads `SynthesisRequest` frames and writes `SynthesisChunk` frames of 22050 Hz mono S16LE ending with `end_of_utterance`. Python processors must call `claim_stdout()` before importing libraries that print. Do not provide fake inference fallbacks; missing processor configuration is a startup error.
- Prosody values are measurements, not guesses: relative pitch/level are against the session baseline, relative rate is against `--reference-rate` syllables per second, and `emotion` stays empty until a classifier sets it. Text whose audio never arrives within `--prosody-timeout` is forwarded without prosody.
- Record instances run ffmpeg with `libopus` and atomically rename a temporary Ogg stream into `QOL_OUTPUT_DIR/<session>-<name>.ogg`. Existing output is treated as a successful retry.
- Event sequence numbers start at zero and are contiguous per session/channel. `Event.Span` carries source-media alignment independently from wall-clock `ProducedAt`.
- Event IDs are deterministic SHA-256 hashes of NUL-separated stage/session/sequence identity.
- Each stage may request a queue group through generic `SubscribeOptions`. The NATS adapter uses core `Subscribe` / `QueueSubscribe`. Durable and replay options are ignored.
- Stream state (STT streams, prosody PCM buffers, utterance sequencing) is process-local. Do not scale a stage horizontally until stream-affinity or shared stream-state coordination is implemented.
- Store deletion is revision-checked. Preserve optimistic concurrency for tickets and sessions.
- Usernames are trimmed/lowercased. Session and ticket secrets are returned only to clients; their storage keys are SHA-256 hashes.
- HTTP payloads, errors, NATS events, disk records, and WebTransport frames are protobuf. Frames are unsigned-varint length-prefixed and capped at 4 MiB.
- WebTransport requires TLS and an exact origin match. `task dev` propagates the self-signed certificate hash to the browser.
- Do not use JetStream, durable consumers, or NATS KV.

## Generated code

Never hand-edit `gen/qol/v1/*.pb.go`, `e2e/processors/gen/**`, or `web/src/gen/qol/v1/qol_pb.ts`. `buf generate` updates Go and Python; Python bindings need `protobuf>=7.36.1,<8` at runtime. Regenerate TypeScript with `protoc-gen-es` after schema changes and verify both trees are synchronized.

## Testing patterns

- Go tests use the standard library and small fakes rather than mocking frameworks.
- Worker tests feed multiple chunks, assert EOS/order, stage subjects, language/media transitions, lineage, deterministic IDs, and stored bytes.
- Wire tests round-trip every payload and chunk metadata and cover validation failures.
- Web transport tests stub global `WebTransport`; Vitest runs under Node, not a browser.
- Unit tests do not cover live NATS redelivery, model executables, ffmpeg encoding, TLS negotiation, or full upload completion. Use the real development stack for those paths.
- Prosody tests synthesize harmonic tones, glides, level steps, and silences rather than recorded speech; the real graph test covers recorded speech.
