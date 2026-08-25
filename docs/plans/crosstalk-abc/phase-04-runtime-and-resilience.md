# Phase 4: Runtime Wiring and Resilience

## Goal

Expose the `crosstalk` stage through Qol's production worker CLI and development deployment, secure its token, and prove reconnect, reassignment, readiness, and failure behavior under the real NATS adapter. After this phase an operator can run the integration without custom code.

## BDD Success Criteria

### Scenario: Operator configures a full-duplex stage

- **Given** a provisioned ABC URL/token and chosen Qol channels/profiles
- **When** `qol-worker run --type crosstalk` starts through flags or `QOL_*` environment variables
- **Then** it validates configuration before connecting
- **And** reports readiness only after NATS, ABC control, assignment, codec, and enabled media directions are ready
- **And** secrets remain redacted.

### Scenario: Direction can be independently disabled

- **Given** source-only or sink-only configuration
- **When** the worker starts
- **Then** it creates only the declared `StageSpec` port and required converter resources
- **And** does not subscribe, publish, or negotiate unnecessary application behavior for the disabled direction.

### Scenario: Reconnect preserves service without mixing epochs

- **Given** a running worker and a temporary WebSocket/ICE/server failure
- **When** Crosstalk becomes available again
- **Then** the worker reconnects with capped exponential backoff and jitter
- **And** closes the old Qol epoch before the new one
- **And** readiness reflects the outage
- **And** buffered stale audio is not replayed.

### Scenario: Assignment and authorization remain server-controlled

- **Given** an unassigned ABC, later assignment, monitor change, or token deletion
- **When** Crosstalk changes the peer lifecycle
- **Then** Qol does not produce or consume unauthorized media while unassigned
- **And** reconnects into only the server-returned assignment/monitor
- **And** stops retrying or clearly degrades on non-retryable authentication failure.

### Scenario: NATS pressure is diagnosable

- **Given** audio faster than the configured converter/WebRTC path
- **When** the bounded queue reaches capacity
- **Then** the configured drop-or-fail policy is applied per complete frame
- **And** logs/metrics expose direction, count, and epoch
- **And** process memory remains bounded.

## Implementation Instructions

1. Extend `cmd/qol-worker/main.go` with `crosstalk` type and explicit flags/environment bindings for server URL, token, source output channel/profile/rate/channels, sink input accepted profiles, queue sizes/drop policy, reconnect bounds, and ffmpeg. Avoid ambiguous reuse of record `--input/--output` help without direction descriptions.
2. Keep secrets out of Cobra usage defaults, process logs, stage specs, NATS events, health payloads, and compose files committed to Git. Support `_FILE` or equivalent file-based token injection for containers; direct environment support may remain for local development with warnings/documentation.
3. Add a dedicated runtime image containing Qol worker, ffmpeg, and any required Pion/codec runtime. Run non-root and provide only necessary network/filesystem permissions.
4. Add an opt-in development-compose service/profile rather than requiring Crosstalk for ordinary `task dev`. Document external Crosstalk URL reachability from Docker and secret provisioning.
5. Add readiness/liveness semantics appropriate to the existing runtime. Liveness means process/event loop healthy; readiness requires NATS and a valid assigned ABC media epoch for enabled directions. If no health endpoint exists for workers, provide structured state and a container health command rather than inventing false readiness.
6. Implement reconnect classification: invalid/revoked token and incompatible protocol/codec are terminal or slow/manual-retry; transient HTTP, WebSocket, ICE, and server restart failures use capped jittered backoff. Reset backoff after a stable connection.
7. Handle `RestartCommand`, server peer closure on assignment/monitor changes, SIGTERM, NATS disconnect/reconnect, and converter failure consistently. One direction's failure policy must be explicit: restart the complete epoch by default to avoid half-stale duplex state.
8. Improve only the NATS seams required for correctness: expose publish/subscription failures needed by this stage, and do not claim delivery guarantees the core NATS adapter lacks. Real-time events remain live and non-durable.
9. Add telemetry inventory for configured Crosstalk channels rather than relying exclusively on the existing fixed `graphChannels()` list. Prefer stage/spec-driven registration if feasible; otherwise document the bounded interim update.
10. Update Qol README/AGENTS with topology, configuration, supported profiles, token handling, session-epoch mapping, non-durable behavior, loop risk, and troubleshooting.

## End-to-End Test Plan

- Run real NATS plus a real Crosstalk server and provisioned ABC, launching the compiled Qol worker process.
- Cover source-only, sink-only, and full-duplex configurations through flags and environment/file secrets.
- Kill/restart Crosstalk, interrupt network connectivity where the harness supports it, change assignment/monitor, delete token, restart NATS, crash ffmpeg, and send SIGTERM.
- Assert readiness/log transitions, capped retry timing, EOS/new epoch IDs, no stale replay, bounded queues, no orphan processes, and no token in captured output.
- Verify another subscriber on the same Qol producer channel still receives every live event while the Crosstalk stage runs.
- Required Qol gates:

```text
go test ./...
go vet ./...
go build ./cmd/...
npm --prefix web test -- --run
```

- Run the compose smoke path with the opt-in Crosstalk profile and a real external/test server.

## Anti-Cheating Audit

- Inspect the built container/process identity, installed codec runtime, secret mounts, and effective command rather than trusting compose text.
- Confirm readiness changes are driven by live NATS/ABC/codec state, not a startup boolean.
- Search logs, events, process arguments, compose renderings, and test artifacts for the sentinel token.
- Confirm reconnect tests actually terminate live WebSocket/ICE/server resources and do not call an internal reconnect method.
- Monitor RSS and goroutine/process counts during pressure/reconnect loops.
- Verify no test-only bypass allows unassigned ABC media or disables TLS/auth validation.
- Confirm NATS tests use the concrete adapter and that fan-out behavior is observed by an independent subscriber.

## Completion Gate

- [ ] Compiled worker runs all three direction modes through supported configuration sources.
- [ ] Token handling is redacted and container-secret capable.
- [ ] Non-root runtime image and opt-in compose wiring work.
- [ ] Readiness, reconnect, reassignment, revocation, NATS outage, converter crash, and shutdown scenarios pass.
- [ ] Queue/memory/process behavior remains bounded under pressure and repeated reconnects.
- [ ] Concrete NATS fan-out is proven.
- [ ] Qol tests, vet, build, web tests, and runtime smoke pass.
