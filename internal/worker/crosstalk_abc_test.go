package worker

import (
	"testing"
	"time"
)

func TestNextBackoffStaysWithinBounds(t *testing.T) {
	cfg := CrosstalkConfig{MinBackoff: 100 * time.Millisecond, MaxBackoff: 400 * time.Millisecond}
	seen := map[time.Duration]bool{}
	current := time.Duration(0)
	for range 12 {
		current = nextBackoff(cfg, current)
		if current < cfg.MinBackoff/2 || current > cfg.MaxBackoff {
			t.Fatalf("backoff %s outside [%s, %s]", current, cfg.MinBackoff/2, cfg.MaxBackoff)
		}
		seen[current] = true
	}
	if len(seen) < 2 {
		t.Fatal("expected jittered backoff values")
	}
}

func TestIsTerminalABCError(t *testing.T) {
	if isTerminalABCError(nil) {
		t.Fatal("nil is not terminal")
	}
	if !isTerminalABCError(errString("unsupported codec")) {
		t.Fatal("unsupported codec should be terminal")
	}
	if isTerminalABCError(errString("connection reset")) {
		t.Fatal("transient error should not be terminal")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
