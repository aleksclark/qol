# Qol

Qol is a streaming voice translation system. The name is the Hebrew word for voice.

## Graph

1. The administrator or `qol-feed` streams an audio file or live microphone capture through WebTransport onto `capture`.
2. `capture` fans each source chunk onto STT and the English record instance.
3. `stt-whisper` consumes ordered audio chunks and emits English text chunks.
4. `translate` consumes English text and emits Spanish text.
5. `tts` consumes Spanish text and emits Spanish PCM audio.
6. Two `record` instances write Ogg/Opus: `record-en` from the captured source, `record-es` from TTS.

Every worker conforms to the generic `Stage` contract in `core.go`: `Spec()` declares typed ports and `Run(ctx, bus)` uses the transport-neutral `Bus`. The `io.Reader`/`io.Writer` processor interface sits behind that boundary. Core NATS carries generic protobuf event envelopes; a stage may spool events before invoking a model that requires complete input.

## Services

- `qol-api`: protobuf HTTP control plane and WebTransport upload gateway
- `qol-worker`: one generic `Stage` selected with `--type` and, for `record`, `--name` / `--input` / `--output`. `--type crosstalk` is the optional ABC bridge.
- `qol-feed`: CLI that emulates the administrator upload through WebTransport
- `web`: React administrator interface
- NATS: core publish/subscribe event transport

## Development

Start the complete development stack:

```sh
task dev
```

The STT, translation, and TTS services each build a dedicated image containing their processor runtime and model. No host processor executables or model mounts are required. The initial build downloads the streaming Zipformer and English-to-Spanish models; later builds use Docker's layer cache.

The development stack generates a short-lived P-256 certificate, provisions the administrator, and prints the web URL and credentials. Override credentials with `QOL_ADMIN_USERNAME` and `QOL_ADMIN_PASSWORD`.

Stream a file through the same path as the SPA. `qol-feed` paces chunks at the file's playback duration so the session stays live like a microphone:

```sh
go run ./cmd/qol-feed http://localhost:9875 --username admin --password change-me sermon_sample.mp3
```

Recorded English and Spanish Ogg/Opus files are written to `tmp/`.

## Crosstalk ABC

`crosstalk` is an optional bidirectional stage. It authenticates to a Crosstalk server as an Audio Booth Connector, publishes monitor audio onto a Qol source channel, and/or sinks a Qol producer channel back through the ABC send track.

```sh
qol-worker run --type crosstalk \
  --crosstalk-url https://crosstalk.example \
  --crosstalk-token-file /run/secrets/crosstalk_token \
  --crosstalk-source-channel graph.stt-whisper.input \
  --crosstalk-sink-channel graph.record-es.input \
  --crosstalk-output-profile ogg-opus
```

`--crosstalk-url` is the HTTP origin. The ABC client appends `/ws/signaling`. Do not pass the full WebSocket path.

- Source-only and sink-only work by omitting the unused channel. Input and output channels must differ.
- Output profile is explicit: `pcm-s16le` / `pcm-f32le` with `--crosstalk-output-rate` / `--crosstalk-output-channels`, or `ogg-opus`.
- Prefer `QOL_CROSSTALK_TOKEN_FILE`. An env/flag token is accepted for local use and logs a warning. The token never appears in usage defaults, events, health JSON, or logs.
- Each assigned ABC epoch maps to Qol session `assigned/peer/epoch`. Reconnects emit EOS and start a new sequence space.
- Default sink subscription is live fan-out. Competing-consumer mode requires `--crosstalk-competing`.
- NATS delivery is live and non-durable. Readiness is a health file (`qol-worker health` exits 0 only when assigned and converting). Reasons include `connecting`, `unassigned`, `retrying`, `auth`, `protocol`, `unsupported-codec`.
- Ordinary `task dev` does not start Crosstalk. Opt in with `COMPOSE_PROFILES=crosstalk`, a reachable `QOL_CROSSTALK_URL` (often `host.docker.internal`), and a token file at `.dev-certs/crosstalk.token` or `QOL_CROSSTALK_TOKEN_FILE`.
- `--crosstalk-disable-mdns` and `--crosstalk-disable-stun` are localhost ICE helpers. Leave them off in production.
- Admin/`ct-play` can publish into a `broadcast` channel, not a `feed`. For Crosstalk→Qol, set the ABC monitor to that broadcast. For Qol→Crosstalk, leave the monitor unset so a feed listener hears the ABC produce track.
- Acoustic loop is still possible if Crosstalk monitors the same room the ABC feed plays into.
- Compiled PCM, encoded, duplex, isolation, and SIGTERM proofs are skip-by-default. With a live `ct-server`, run `QOL_CROSSTALK_E2E=1 task test:e2e:crosstalk-qol`. Ordinary `task test` does not start Crosstalk.

## Verification

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
