# Review Cleanup Plan

## Outcome

Completing this plan makes the Crosstalk bridge safe across multiple Qol sessions, exposes converter failures instead of silently losing audio, enforces the encoded-media contract, restores terminal NATS subscription semantics, derives encoded output timing from media, and returns Go module metadata to canonical form.

## Current-State Summary

- The graph terminology migration and Crosstalk ABC bridge are implemented on `feature/graph-terminology`.
- Go tests, focused race tests, `go vet`, command builds, protobuf lint, and web tests/typecheck/build pass.
- Live Crosstalk proofs exist but require an external Crosstalk server and are skipped by the ordinary test gate.
- Review found five behavioral defects in media/session/subscription handling and one module-maintenance defect.
- The only unrelated working-tree item at review time is the untracked `sermon_sample.mp3`; cleanup work must not modify or add it.

## Scope Boundaries

### In scope

- Reusable outbound conversion across sequential Qol sessions in one ABC epoch.
- Converter startup/runtime error propagation and accurate health transitions.
- Correct encoded input media-type validation and demuxing.
- Terminal NATS subscription behavior after decode or handler failure.
- Media-derived spans for Ogg/Opus output.
- Canonical `go.mod` and `go.sum` metadata.
- Focused regression tests and real-boundary verification for every correction.

### Out of scope

- New codecs or media profiles beyond those the implementation can truthfully support.
- Durable NATS delivery, JetStream, or replay semantics.
- Changes to Crosstalk authorization, assignment, or mixer behavior.
- Graph subject migration compatibility, inline-token policy, monitor-track renegotiation, and shutdown publish deadlines unless separately planned.
- Modifying the untracked sample audio file.

## Global Constraints

- Keep wire and persisted representations protobuf-based.
- Preserve live core-NATS fan-out and explicit competing-consumer behavior.
- Do not report readiness while an enabled media direction has terminated.
- Do not advertise a media type unless production conversion and decoded-content tests prove it.
- Use media clocks or container timing for spans, never wall-clock scheduling or arbitrary pipe read boundaries.
- Keep queues bounded, propagate terminal errors, reap subprocesses, and avoid stale audio across sessions or epochs.
- Run focused tests after each change, then the repository gates documented in `AGENTS.md`.
- Live Crosstalk completion evidence must use a real server and compiled worker; unit fakes cannot substitute for it.

## Phase Overview

| Phase | Goal | Depends on |
|---|---|---|
| [Phase 1: Sequential sink sessions](./phase-01-sequential-sink-sessions.md) | Keep Qol-to-Crosstalk audio working after one session reaches EOS | None |
| [Phase 2: Converter failure propagation](./phase-02-converter-failure-propagation.md) | Turn asynchronous conversion failures into epoch and health failures | Phase 1 |
| [Phase 3: Encoded input contract](./phase-03-encoded-input-contract.md) | Demux every accepted encoded type correctly or reject it synchronously | Phase 2 |
| [Phase 4: Terminal NATS subscriptions](./phase-04-terminal-nats-subscriptions.md) | Align adapter behavior with the `Subscription` terminal-error contract | None |
| [Phase 5: Encoded media spans](./phase-05-encoded-media-spans.md) | Derive Ogg/Opus spans from media timing rather than pipe reads | Phase 2 |
| [Phase 6: Module metadata](./phase-06-module-metadata.md) | Restore canonical dependency classification and checksums | Phases 1-5 |

## Completion Rule

The cleanup is complete only when every phase checklist passes, focused and full local gates are green, `go mod tidy -diff` is empty, sequential real audio sessions and forced converter failures are proven through production wiring, encoded media timing matches decoded duration within an explicit tolerance, and the anti-cheating audits find no bypasses or swallowed failures.
