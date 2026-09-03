# Phase 6: Module Metadata

## Goal

Return Go dependency metadata to the canonical form produced by the pinned Go toolchain. `go mod tidy -diff` currently reports `github.com/nats-io/nats-server/v2` and `nhooyr.io/websocket` as incorrectly indirect and identifies stale `golang.org/x/time` checksums.

## BDD Success Criteria

### Scenario: Module metadata is canonical

- **Given** the repository's declared Go 1.26.6 toolchain and current source/tests
- **When** `go mod tidy -diff` runs
- **Then** it exits successfully with no diff
- **And** dependencies imported by repository tests or production code are classified correctly.

### Scenario: Cleanup does not change runtime behavior

- **Given** only dependency classification and stale checksums need correction
- **When** metadata is tidied
- **Then** selected module versions remain unchanged unless a separately reviewed compatibility need requires a change
- **And** all Go tests, vet, and command builds retain their prior behavior.

## Implementation Instructions

1. Run the pinned Go 1.26.6 toolchain's `go mod tidy`; review the exact `go.mod` and `go.sum` diff before retaining it.
2. Confirm `github.com/nats-io/nats-server/v2` is a direct test dependency from `internal/eventbus/nats_test.go` and `nhooyr.io/websocket` is directly imported by repository code/tests. Keep direct classification even if usage is test-only because `go.mod` does not separate test requirements.
3. Remove only checksums proven stale by tidy. Do not opportunistically upgrade modules or alter dependency versions.
4. Run dependency and vulnerability tooling already available in the repository environment; do not add a new toolchain requirement solely for this phase.
5. Re-run generated-code checks to ensure tidy did not mask missing generated imports.
6. Run:

```text
go mod tidy -diff
go test ./...
go vet ./...
go build ./cmd/...
```

## End-to-End Test Plan

- Build all three commands from a clean module cache or the project's standard Docker build environment so dependency resolution is proven outside the warm developer cache.
- Run the Compose startup e2e gate documented in `AGENTS.md` to ensure module metadata supports container builds:

```text
docker compose -f docker-compose.e2e.yml up --build --abort-on-container-exit --exit-code-from e2e
docker compose -f docker-compose.e2e.yml down
```

- No fake dependency registry or vendored substitute is permitted.

## Anti-Cheating Audit

- Inspect the diff for version upgrades, replacements, exclusions, or unrelated checksum churn.
- Confirm `go mod tidy -diff` itself is empty; a passing build alone is insufficient.
- Confirm direct requirements correspond to actual repository imports and were not manually moved merely to silence diagnostics.
- Verify builds resolve from declared metadata rather than an uncommitted workspace or vendor directory.
- Check repository status so `sermon_sample.mp3` and unrelated files remain untouched.

## Completion Gate

- [ ] `go mod tidy -diff` is empty under Go 1.26.6.
- [ ] No unplanned module version changes occurred.
- [ ] Stale checksums are removed.
- [ ] Full Go tests, vet, and command builds pass.
- [ ] Clean-environment/container dependency resolution passes.
- [ ] Repository status contains only intended plan or implementation changes.
