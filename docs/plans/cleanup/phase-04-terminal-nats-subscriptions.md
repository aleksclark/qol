# Phase 4: Terminal NATS Subscriptions

## Goal

Align the concrete NATS adapter with the `qol.Subscription` contract. Decode and handler failures currently set `Err` but leave delivery active and `Done` open, so callers waiting for terminal subscription state can block forever while more messages continue arriving.

## BDD Success Criteria

### Scenario: Handler failure terminates delivery

- **Given** a live core-NATS subscription
- **When** its handler returns an error
- **Then** `Err` preserves that error
- **And** `Done` closes promptly
- **And** the NATS subscription stops delivering later messages.

### Scenario: Decode failure terminates delivery

- **Given** malformed protobuf bytes on a subscribed subject
- **When** the adapter cannot decode an event
- **Then** the subscription terminates with the decode error
- **And** no later valid message is delivered through that subscription.

### Scenario: Cancellation remains clean

- **Given** a healthy subscription
- **When** its context is canceled or `Close` is called
- **Then** `Done` closes exactly once
- **And** `Err` follows the documented cancellation policy
- **And** concurrent close/error paths do not race or deadlock.

## Implementation Instructions

1. Refactor `internal/eventbus/nats.go` so recording a terminal callback error also initiates one idempotent unsubscribe/close path.
2. Avoid deadlocks inside a NATS callback. Do not hold the subscription error mutex while unsubscribing or closing `done`.
3. Preserve the first non-cancellation terminal error. Concurrent context cancellation must not erase a handler/decode failure.
4. Ensure `Close`, context cancellation, decode failure, and handler failure converge through one `sync.Once` lifecycle while still returning relevant unsubscribe errors where possible.
5. Expand `internal/eventbus/nats_test.go` to assert `Done` closure, exact `Err`, and absence of delivery after failure. Include malformed protobuf publication through the underlying NATS connection.
6. Review worker callers waiting on `Done()` to ensure the corrected behavior propagates stage failure as intended.
7. Run:

```text
go test ./internal/eventbus ./internal/worker
go test -race ./internal/eventbus ./internal/worker
```

## End-to-End Test Plan

- Start a real NATS server and compiled worker using the production adapter.
- Publish an event that causes a deterministic stage handler failure, then publish a valid later event on the same subject.
- Assert the worker observes terminal subscription failure, the later event is not processed by that subscription, and shutdown/restart behaves predictably.
- Publish malformed protobuf directly to a dedicated production subject and assert the same terminal behavior without crashing NATS or unrelated subscriptions.
- Run `go test ./internal/eventbus ./internal/worker` against both the embedded test server and `NATS_URL` pointing to a standalone server.

## Anti-Cheating Audit

- Confirm tests inspect both `Err` and `Done`, then publish another message to prove delivery stopped.
- Confirm malformed input is sent through NATS rather than passed directly to `decode`.
- Inspect lock ordering and callback behavior for unsubscribe deadlocks.
- Verify no polling-only workaround substitutes for correct channel closure.
- Confirm unrelated subscriptions continue receiving messages after one subscription fails.

## Completion Gate

- [ ] Handler errors close `Done` and stop delivery.
- [ ] Decode errors close `Done` and stop delivery.
- [ ] The first terminal error is preserved.
- [ ] Cancellation and explicit close remain idempotent.
- [ ] Independent subscriptions remain unaffected.
- [ ] Focused tests and race tests pass.
