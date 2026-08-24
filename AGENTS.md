# Qol agent guide

Qol is a streaming voice translation system with a Go control plane and stage workers, a React administrator UI, and core NATS for event transport. Persistence is a local disk store. Keep all wire and persisted representations protobuf-based.

## Commands

- Focused Go package: `go test ./internal/<package>`
- All Go tests: `task test` or `go test ./...`
- Web tests: `task web-test` or `npm --prefix web test -- --run`
- Web typecheck: `npm --prefix web run typecheck`
- Protobuf lint/generation: `buf lint && buf generate`
- Development stack: run `task dev`. Dedicated worker images contain their processor dependencies and models.
- Stop development: `task dev:down`; API data and the graph spool persist. Recorded Ogg/Opus files land in gitignored `tmp/`.

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
- `cmd/qol-worker`: composition root for one stage selected by `--type=capture|stt-whisper|translate|tts|record`. `--name`, `--input`, and `--output` distinguish record instances.
- `cmd/qol-feed`: CLI that logs in, takes an upload ticket, and streams a file through WebTransport at playback rate like a live microphone.
- Root package (`core.go`, `payloads.go`, `types.go`): transport-independent generic event, bus, stage, port, replay, and semantic payload contracts.
- `internal/api`: protobuf HTTP auth and WebTransport upload. It publishes uploaded chunks to the capture subject and waits for both record completion events.
- `internal/worker`: `qol.Stage` implementations, stream aggregation behind the generic bus boundary, subprocess `io.Reader`/`io.Writer` adapters, and atomic Ogg/Opus storage.
- `internal/eventbus`: core NATS publish/subscribe, queue groups, and protobuf serialization.
- `internal/wire`: the only domain/protobuf conversion layer plus varint-delimited framing.
- `internal/auth` and `internal/store`: users, sessions, one-use upload tickets, and memory/disk adapters.
- `proto/qol/v1`: source wire schema. Generated Go is in `gen/qol/v1`; generated browser code is in `web/src/gen/qol/v1`.

Graph subjects are fixed:

1. `qol.graph.capture.input`: source audio chunks from the API.
2. `qol.graph.capture.output`: canonical capture fan-out copy.
3. `qol.graph.stt-whisper.input`: source audio for STT.
4. `qol.graph.record-en.input`: source audio for the English record instance.
5. `qol.graph.translate.input`: UTF-8 English text chunks.
6. `qol.graph.tts.input`: UTF-8 Spanish text chunks.
7. `qol.graph.record-es.input`: Spanish S16LE PCM chunks.
8. `qol.graph.record-en.completed`: English Ogg/Opus metadata.
9. `qol.graph.record-es.completed`: Spanish Ogg/Opus metadata.

Capture publishes each incoming audio-stream event onto the three capture output ports. STT and `record-en` subscribe to their own input subjects.

Every externally visible worker implements `qol.Stage` through `Spec()` and `Run(ctx, bus)`. Stage ports declare channels and accepted/produced semantic event types. Internal processors consume `io.Reader` and produce `io.Writer`; the Ogg/Opus store consumes `io.Reader`. The current runner may spool a stream under `QOL_SPOOL_DIR` behind the Stage/Bus contracts before invoking a processor.

## Invariants and gotchas

- Processor executables read stdin and write stdout. STT streams UTF-8 English lines as utterances finalize, translation emits one Spanish utterance per English event, and TTS emits raw 22050 Hz mono S16LE PCM. Do not provide fake inference fallbacks; missing processor configuration is a startup error.
- Record instances run ffmpeg with `libopus` and atomically rename a temporary Ogg stream into `QOL_OUTPUT_DIR/<session>-<name>.ogg`. Existing output is treated as a successful retry.
- Event sequence numbers start at zero and are contiguous per session/channel. `Event.Span` carries source-media alignment independently from wall-clock `ProducedAt`.
- Event IDs are deterministic SHA-256 hashes of NUL-separated stage/session/sequence identity.
- Each stage may request a queue group through generic `SubscribeOptions`. The NATS adapter uses core `Subscribe` / `QueueSubscribe`. Durable and replay options are ignored.
- Local spool state is process-local despite a shared mounted volume. Do not scale a stage horizontally until stream-affinity or shared stream-state coordination is implemented.
- Store deletion is revision-checked. Preserve optimistic concurrency for tickets and sessions.
- Usernames are trimmed/lowercased. Session and ticket secrets are returned only to clients; their storage keys are SHA-256 hashes.
- HTTP payloads, errors, NATS events, disk records, and WebTransport frames are protobuf. Frames are unsigned-varint length-prefixed and capped at 4 MiB.
- WebTransport requires TLS and an exact origin match. `task dev` propagates the self-signed certificate hash to the browser.
- Do not use JetStream, durable consumers, or NATS KV.

## Generated code

Never hand-edit `gen/qol/v1/*.pb.go` or `web/src/gen/qol/v1/qol_pb.ts`. `buf generate` updates Go only. Regenerate TypeScript with `protoc-gen-es` after schema changes and verify both trees are synchronized.

## Testing patterns

- Go tests use the standard library and small fakes rather than mocking frameworks.
- Worker tests feed multiple chunks, assert EOS/order, stage subjects, language/media transitions, lineage, deterministic IDs, and stored bytes.
- Wire tests round-trip every payload and chunk metadata and cover validation failures.
- Web transport tests stub global `WebTransport`; Vitest runs under Node, not a browser.
- Unit tests do not cover live NATS redelivery, model executables, ffmpeg encoding, TLS negotiation, or full upload completion. Use the real development stack for those paths.
