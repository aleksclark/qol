package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

func writeFixture(t *testing.T, transfer bool) (string, string) {
	t.Helper()
	var requests, audio bytes.Buffer
	format := qol.PCMFormat{SampleRate: 22050, Channels: 1, Format: qol.SampleS16LE}
	for _, relative := range []float64{0.8, 1.0, 1.2, 1.4} {
		if err := wire.WriteSynthesisRequest(&requests, qol.SynthesisRequest{Text: "uno dos tres cuatro", Language: "es", Prosody: &qol.Prosody{Rate: float32(4 * relative), RelativeRate: float32(relative)}}); err != nil {
			t.Fatal(err)
		}
		seconds := 2.0
		if transfer {
			seconds /= relative
		}
		pcm := make([]byte, int(seconds*22050)*2)
		for index := 0; index < len(pcm)/2; index++ {
			phase := 2 * math.Pi * 140 * float64(index) / 22050
			binary.LittleEndian.PutUint16(pcm[index*2:], uint16(int16(8000*math.Sin(phase))))
		}
		if err := wire.WriteSynthesisChunk(&audio, qol.SynthesisChunk{Audio: qol.Audio{Format: format, Data: pcm}, EndOfUtterance: true}); err != nil {
			t.Fatal(err)
		}
	}
	directory := t.TempDir()
	requestsPath, audioPath := filepath.Join(directory, "requests.bin"), filepath.Join(directory, "audio.bin")
	if err := os.WriteFile(requestsPath, requests.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audioPath, audio.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return requestsPath, audioPath
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := newCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), err
}

func TestCarryThroughGatesSpeakingRate(t *testing.T) {
	requests, audio := writeFixture(t, true)
	output, err := run(t, "carry-through", "--requests", requests, "--audio", audio, "--min-rate-correlation", "0.9", "--verbose")
	if err != nil || !strings.Contains(output, "correlation rate=1.00") || strings.Count(output, "src rate=") != 4 {
		t.Fatalf("expected a passing report, got %v:\n%s", err, output)
	}
	requests, audio = writeFixture(t, false)
	if output, err := run(t, "carry-through", "--requests", requests, "--audio", audio, "--min-rate-correlation", "0.5"); err == nil {
		t.Fatalf("constant-rate output must fail the gate:\n%s", output)
	}
}
