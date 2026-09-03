# Phase 2: Converter Failure Propagation

## Goal

Make enabled media directions fail visibly when FFmpeg cannot start or exits asynchronously. `Inbound.Start` and `Outbound.Start` currently return before process startup, while closed output channels are interpreted as normal completion and health can remain `ready`. This phase makes converter state part of epoch readiness and lifecycle.

## BDD Success Criteria

### Scenario: Converter cannot start

- **Given** a configured Crosstalk direction with a missing or non-executable FFmpeg command
- **When** an ABC epoch starts
- **Then** readiness never becomes true for that epoch
- **And** health reports a non-secret conversion failure reason
- **And** the epoch is closed or retried according to configured policy.

### Scenario: Converter exits during media

- **Given** an assigned, ready epoch carrying audio
- **When** FFmpeg exits unexpectedly
- **Then** the converter exposes its terminal error to the stage
- **And** health leaves `ready`
- **And** the complete epoch restarts by default so duplex state cannot remain half-stale
- **And** process-exit and direction error metrics increment.

### Scenario: Normal EOS remains distinguishable

- **Given** valid media and an intentional stream EOS or context cancellation
- **When** conversion finishes
- **Then** the stage does not misclassify normal completion as a crash
- **And** cleanup reaps FFmpeg and closes channels exactly once.

## Implementation Instructions

1. Add an explicit readiness/error contract to `internal/media.Inbound` and `internal/media.Outbound`. It must distinguish process-start success, normal EOS, cancellation, and terminal conversion error without requiring callers to infer meaning from channel closure.
2. In `internal/worker/crosstalk.go`, wait for enabled converters to reach operational readiness before setting `CrosstalkHealth.Ready` to true.
3. Feed asynchronous converter errors into the epoch error channel. A failed enabled direction must cancel and clean up the complete epoch unless a different documented policy is introduced.
4. Preserve the first meaningful terminal error. Do not overwrite process failure with context cancellation during cleanup.
5. Update `Close` behavior so callers can observe errors; remove tests that intentionally discard the only error evidence when the behavior under test is failure propagation.
6. Ensure health JSON and logs include stage, direction, epoch, and a sanitized reason but never command secrets or token values.
7. Add tests with a nonexistent executable, a command that exits immediately, and a process that emits media before failing.
8. Run:

```text
go test ./internal/media ./internal/worker ./cmd/qol-worker
go test -race ./internal/media ./internal/worker ./cmd/qol-worker
```

## End-to-End Test Plan

- Launch compiled sink-only, source-only, and duplex workers against real NATS/Crosstalk with a controlled FFmpeg wrapper that starts and then exits nonzero.
- Assert the health command transitions from connecting/ready to a conversion failure, the ABC epoch disconnects, and no direction continues silently.
- Restore a working FFmpeg command or restart the worker and verify audio resumes in a new epoch without stale frames.
- Capture child processes before and after failure to prove they are reaped.
- Extend and run `QOL_CROSSTALK_E2E=1 task test:e2e:crosstalk-qol`.

## Anti-Cheating Audit

- Confirm readiness waits for real converter startup rather than a goroutine launch or allocated channel.
- Confirm forced failures execute the production subprocess path and are not injected directly into private fields.
- Search for ignored `Close`/`Wait` errors and channel closures treated unconditionally as success.
- Verify health assertions are paired with absence/presence of decoded audio.
- Check retry loops are bounded and do not hide permanent failures with broad retries.

## Completion Gate

- [ ] Startup failures prevent readiness.
- [ ] Runtime failures terminate or restart the complete epoch.
- [ ] Normal EOS and cancellation remain non-error paths.
- [ ] First terminal errors survive cleanup.
- [ ] Health, logs, and metrics diagnose the failed direction without secrets.
- [ ] Focused tests, race tests, and compiled live proofs pass.
