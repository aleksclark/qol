package main

import (
	"bufio"
	"bytes"
	"testing"
	"time"

	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"github.com/aleksclark/qol/internal/wire"
)

func TestWriteAudioChunksSendsSeparateEndFrame(t *testing.T) {
	var encoded bytes.Buffer
	if err := writeAudioChunks(&encoded, bytes.NewReader([]byte("abc")), 3, 0); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(&encoded)
	first := new(qolv1.ClientFrame)
	if err := wire.ReadFrame(reader, first); err != nil {
		t.Fatal(err)
	}
	if first.GetAudioChunk() == nil || first.GetAudioChunk().EndOfStream || !bytes.Equal(first.GetAudioChunk().Data, []byte("abc")) {
		t.Fatalf("unexpected data chunk: %#v", first)
	}
	second := new(qolv1.ClientFrame)
	if err := wire.ReadFrame(reader, second); err != nil {
		t.Fatal(err)
	}
	if second.GetAudioChunk() == nil || !second.GetAudioChunk().EndOfStream || len(second.GetAudioChunk().Data) != 0 || second.GetAudioChunk().Sequence != 1 {
		t.Fatalf("unexpected end chunk: %#v", second)
	}
}

func TestWriteAudioChunksEmptyFileStillSendsEnd(t *testing.T) {
	var encoded bytes.Buffer
	if err := writeAudioChunks(&encoded, bytes.NewReader(nil), 0, 0); err != nil {
		t.Fatal(err)
	}
	frame := new(qolv1.ClientFrame)
	if err := wire.ReadFrame(bufio.NewReader(&encoded), frame); err != nil {
		t.Fatal(err)
	}
	if frame.GetAudioChunk() == nil || !frame.GetAudioChunk().EndOfStream || frame.GetAudioChunk().Sequence != 0 {
		t.Fatalf("unexpected empty end: %#v", frame)
	}
}

func TestPaceDelayMatchesPlaybackRate(t *testing.T) {
	if got := paceDelay(0, 32*1024, 64*1024, 2*time.Second); got != time.Second {
		t.Fatalf("got %s, want 1s", got)
	}
	if got := paceDelay(200*time.Millisecond, 32*1024, 64*1024, 2*time.Second); got != 800*time.Millisecond {
		t.Fatalf("got remaining %s, want 800ms", got)
	}
	if got := paceDelay(2*time.Second, 32*1024, 64*1024, 2*time.Second); got != 0 {
		t.Fatalf("got %s, want no delay once wall time catches up", got)
	}
	if got := paceDelay(0, 1, 0, time.Second); got != 0 {
		t.Fatalf("got %s, want no delay without a size", got)
	}
}

func TestFallbackDurationUses128kbps(t *testing.T) {
	if got := fallbackDuration(16000); got != time.Second {
		t.Fatalf("got %s, want 1s", got)
	}
}
