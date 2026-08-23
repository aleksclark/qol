# Qol agent guide

Qol is a voice-event processing system with a Go control plane and worker, a React administrator UI, and NATS JetStream as both event transport and KV persistence. Keep wire and persisted representations protobuf-based.

## Commands

### Local development

- `task dev` generates a short-lived P-256 localhost certificate, starts the Compose stack, scales `qol-worker` to two replicas, waits for the API, bootstraps the administrator, and prints the dynamically mapped web URL and credentials.
- Override bootstrap credentials with `QOL_ADMIN_USERNAME` and `QOL_ADMIN_PASSWORD`.
- `task dev:down` removes the development stack. The `nats-dev` named volume persists unless explicitly removed.
- The development host ports are API `9875`, web `9876`, and WebTransport UDP `4443`. The UI's browser-facing origins are configured in `docker-compose.dev.yml`.

### Focused verification

- Go package: `go test ./internal/<package>`
- All Go tests: `task test` or `go test ./...`
- Web unit tests: `task web-test` or `npm --prefix web test -- --run`
- Web typecheck: `npm --prefix web run typecheck`
- Protobuf lint: `buf lint`

### Full local gate

Run the narrowest relevant test first, then:

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
```

The Compose e2e service is currently a startup smoke test: it waits for the API health endpoint and web server, but does not exercise login or audio streaming. Clean it up afterward with `docker compose -f docker-compose.e2e.yml down`.

Tool versions are intentionally newer than many system defaults: Go `1.26.6`, Task `3.52.0`, and the Docker web builds use Node `24.15.0`. Use the lockfile with `npm ci` for reproducible verification; `task web-install` and the dev container use `npm install`.

## Architecture and flow

- `cmd/qol-api`: Cobra entrypoint, NATS/KV dependency wiring, HTTP and HTTP/3 lifecycle, and admin bootstrap.
- `cmd/qol-worker`: Cobra entrypoint that attaches the durable echo consumer.
- `domain`: transport-independent event and PCM types plus narrow interfaces such as `Publisher`.
- `internal/api`: protobuf-over-HTTP authentication endpoints and the WebTransport upload session.
- `internal/auth`: account, cookie-session, and one-use upload-ticket behavior over the abstract store.
- `internal/store`: storage contract; `memory` is the test implementation and `natskv` is production.
- `internal/eventbus`: JetStream setup, protobuf event serialization, subjects, durable consumers, and ack/nak behavior.
- `internal/media`: the ffmpeg subprocess adapter.
- `internal/wire`: the only domain/protobuf conversion layer and length-delimited stream framing.
- `proto/qol/v1`: source-of-truth wire and persisted record schema. Go output is under `gen/qol/v1`; browser output is under `web/src/gen/qol/v1`.
- `web/src/api.ts`: protobuf HTTP client. `transport.ts`: WebTransport framing and upload client. `App.tsx`: login and stream console.

The end-to-end data path is:

1. The browser logs in over protobuf HTTP; the API sets an HTTP-only, strict-same-site session cookie.
2. The browser requests a short-lived, single-use upload ticket because WebTransport setup does not rely on the cookie.
3. The browser opens `/v1/upload`, sends varint-length-prefixed `ClientFrame` messages, and streams MP3 chunks.
4. The API runs ffmpeg and emits 16 kHz, mono, signed 16-bit little-endian PCM in 320-frame chunks to `qol.session.<session>.pcm.input`.
5. Workers queue-consume input events, preserve PCM and sequence/span metadata, link the input as the parent, and publish to `.pcm.output`.
6. The API subscribes to that session's output subject, suppresses duplicate event IDs, and returns framed output events and completion counts.

## Conventions and invariants

- All commands use Cobra and obtain flags/environment through `config.Bind`. A flag such as `--nats-url` maps to `QOL_NATS_URL`; do not add separate ad hoc environment parsing.
- Keep business types in `domain` and generated protobuf types at boundaries. Validate domain data in `internal/wire` before publishing or encoding.
- PCM byte length must equal `frames * channels * 2`. The implemented sample format is only S16LE.
- Event IDs are deterministic SHA-256 hashes of NUL-separated identity parts. JetStream also uses the ID as `Nats-Msg-Id`, so changing ID derivation changes deduplication semantics.
- Event subjects follow `qol.session.<session-id>.pcm.{input,output}`. The event stream retains events for 24 hours and deduplicates publications for two minutes.
- The input subscription is both queue-grouped (`qol-echo`) and durable (`qol-echo-v1`), so the two development workers share work rather than duplicate it. Failed decode/handling is nacked; success is explicitly acked.
- Store deletion is revision-checked. Preserve this optimistic-concurrency behavior for one-use tickets and sessions.
- Usernames are trimmed and lowercased. Raw session and upload-ticket secrets are returned only to clients; storage keys are SHA-256 hashes.
- HTTP payloads, errors, JetStream events, KV records, and WebTransport frames are protobuf. WebTransport frames use unsigned-varint lengths and are capped at 4 MiB.
- WebTransport requires TLS and an exact allowed-origin match. `task dev` propagates the temporary certificate's SHA-256 hash to the browser because the certificate is self-signed.

## Generated code

Treat `gen/qol/v1/*.pb.go` and `web/src/gen/qol/v1/qol_pb.ts` as generated artifacts, not hand-written implementation. `buf generate` currently configures only local `protoc-gen-go`; there is no checked-in TypeScript generation plugin configuration. After schema changes, verify both generated trees remain synchronized and do not assume `buf generate` updates the browser client.

## Testing patterns

- Go tests use the standard library and small in-memory fakes rather than mocking frameworks. Prefer external test packages where access to internals is unnecessary.
- Authentication tests use `internal/store/memory`; preserve behavioral parity between it and NATS KV, especially create conflicts and revision-aware deletes.
- Worker tests inject the narrow `domain.Publisher` interface and assert subjects plus event lineage.
- Wire tests round-trip domain events and cover validation failures.
- Web transport tests stub the global `WebTransport` implementation and assert framed messages. Vitest runs in the Node environment, not a real browser.
- Changes to NATS behavior, ffmpeg integration, TLS/WebTransport negotiation, or the full upload path are not covered by the unit suite; use the development stack for those checks.
