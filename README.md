# Qol

Qol is a voice-event processing system. The name is the Hebrew word for voice.

## Services

- `qol-api`: control plane, protobuf HTTP API, MP3 decoder, and WebTransport gateway
- `qol-worker`: durable NATS JetStream PCM echo handler
- `web`: React administrator interface
- NATS: JetStream event transport and KV persistence

## Development

```sh
task dev
```

This starts the development Compose stack with two workers, generates a short-lived P-256 certificate for WebTransport, provisions the administrator, and prints the login credentials. Override the defaults with `QOL_ADMIN_USERNAME` and `QOL_ADMIN_PASSWORD`.

The task prints the web UI address from the active Compose port mapping. WebTransport requires a TLS certificate and key passed to `qol-api serve`; HTTP login and all other request and stored payloads use protobuf.

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
