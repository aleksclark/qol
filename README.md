# Qol

Qol is a streaming voice translation system. The name is the Hebrew word for voice.

## Graph

1. The administrator or `qol-feed` streams an audio file or live microphone capture through WebTransport onto `capture`.
2. `capture` fans each source chunk onto STT, prosody, and the English record instance.
3. `stt-whisper` consumes ordered audio chunks and emits punctuated English utterances aligned to the source media timeline, committing early at clause boundaries.
4. `prosody` measures each utterance in the source audio (pitch, range, level, speaking rate, pauses, arousal), classifies its emotion, and attaches a `ProsodyPayload`.
5. `translate` consumes annotated English and emits Spanish, carrying the span and prosody forward.
6. `tts` turns each Spanish utterance plus its prosody into Spanish PCM (speed, loudness, pitch level, and pitch range follow the speaker) and places it on the source timeline, so the Spanish track stays aligned with the English one.
7. Two `record` instances write Ogg/Opus: `record-en` from the captured source, `record-es` from TTS.

Every worker conforms to the generic `Stage` contract in `core.go`: `Spec()` declares typed ports and `Run(ctx, bus)` uses the transport-neutral `Bus`. Model processors are long-lived subprocesses with protobuf-framed stdio. Core NATS carries generic protobuf event envelopes.

See [docs/speech-to-speech-roadmap.md](docs/speech-to-speech-roadmap.md) for the model survey, measurements, and next steps.

## Services

- `qol-api`: protobuf HTTP control plane and WebTransport upload gateway
- `qol-worker`: one generic `Stage` selected with `--type` and, for `record`, `--name` / `--input` / `--output`
- `qol-feed`: CLI that emulates the administrator upload through WebTransport
- `web`: React administrator interface
- NATS: core publish/subscribe event transport

## Development

Start the complete development stack:

```sh
task dev
```

The STT, translation, and TTS services each build a dedicated image containing their processor runtime and model. No host processor executables or model mounts are required. The initial build downloads the streaming Zipformer, punctuation, English-to-Spanish (opus-mt-tc-big), emotion2vec+, and Kokoro-82M models; later builds use Docker's layer cache. TTS runs Kokoro on CPU in the development stack.

The development stack generates a short-lived P-256 certificate, provisions the administrator, and prints the web URL and credentials. Override credentials with `QOL_ADMIN_USERNAME` and `QOL_ADMIN_PASSWORD`.

Stream a file through the same path as the SPA. `qol-feed` paces chunks at the file's playback duration so the session stays live like a microphone:

```sh
go run ./cmd/qol-feed http://localhost:9875 --username admin --password change-me sermon_sample.mp3
```

Recorded English and Spanish Ogg/Opus files are written to `tmp/`.

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
