package e2e_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
	"github.com/aleksclark/qol/internal/worker"
)

type memoryBus struct {
	mu       sync.Mutex
	events   map[string][]qol.Event
	handlers map[string][]qol.Handler
}

func newMemoryBus() *memoryBus {
	return &memoryBus{events: make(map[string][]qol.Event), handlers: make(map[string][]qol.Handler)}
}

func (b *memoryBus) Publish(ctx context.Context, event qol.Event) error {
	b.mu.Lock()
	b.events[event.Channel] = append(b.events[event.Channel], event)
	handlers := append([]qol.Handler(nil), b.handlers[event.Channel]...)
	b.mu.Unlock()
	for _, handler := range handlers {
		if err := handler(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func (b *memoryBus) Subscribe(_ context.Context, channel string, _ qol.SubscribeOptions, handler qol.Handler) (qol.Subscription, error) {
	b.mu.Lock()
	b.handlers[channel] = append(b.handlers[channel], handler)
	b.mu.Unlock()
	return &subscription{done: make(chan struct{})}, nil
}

type subscription struct {
	done chan struct{}
	once sync.Once
}

func (s *subscription) Close() error          { s.once.Do(func() { close(s.done) }); return nil }
func (s *subscription) Done() <-chan struct{} { return s.done }
func (s *subscription) Err() error            { return nil }

func TestRealEnglishToSpanishAudioGraph(t *testing.T) {
	if os.Getenv("QOL_RUN_REAL_GRAPH_E2E") == "" {
		t.Skip("set QOL_RUN_REAL_GRAPH_E2E=1 to run local speech models")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	python := os.Getenv("QOL_E2E_PYTHON")
	if python == "" {
		python = "/home/aleks/work/projects/sermon_translate/server/.venv/bin/python"
	}
	outputDir := filepath.Join(root, "e2e-artifacts")
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(outputDir, "sermon_sample_spanish.ogg")
	_ = os.Remove(outputPath)
	_ = os.Remove(filepath.Join(outputDir, "sermon-sample-e2e.ogg"))
	spool := t.TempDir()
	stages := []qol.Stage{
		worker.NewCapture(),
		worker.NewSTT(worker.NewCommandProcessor(python, filepath.Join(root, "e2e/processors/stt_whisper.py")), spool),
		worker.NewTranslate(worker.NewCommandProcessor(python, filepath.Join(root, "e2e/processors/translate_en_es.py")), spool),
		worker.NewTTS(worker.NewCommandProcessor(python, filepath.Join(root, "e2e/processors/tts_spanish.py")), spool),
		worker.NewRecord("record-en", qol.ChannelRecordENInput, qol.ChannelRecordENCompleted, worker.NewFFmpegOggOpusStore("ffmpeg", outputDir), spool),
		worker.NewRecord("record-es", qol.ChannelTTSAudio, qol.ChannelRecordESCompleted, worker.NewFFmpegS16LEOggOpusStore("ffmpeg", outputDir, 22050, 1), spool),
	}
	bus := newMemoryBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, stage := range stages {
		go func(stage qol.Stage) { _ = stage.Run(ctx, bus) }(stage)
	}
	waitForSubscriptions(t, bus, len(stages))
	publishSample(t, ctx, bus, filepath.Join(root, "sermon_sample.mp3"))

	english := textFrom(t, bus, qol.ChannelASRText)
	spanish := translationFrom(t, bus)
	if strings.EqualFold(strings.TrimSpace(english), strings.TrimSpace(spanish)) {
		t.Fatalf("translation did not change the transcript: %q", spanish)
	}
	bus.mu.Lock()
	completed := append([]qol.Event(nil), bus.events[qol.ChannelRecordESCompleted]...)
	bus.mu.Unlock()
	if len(completed) != 1 {
		t.Fatalf("unexpected completion events: %#v", completed)
	}
	stored, err := wire.UnmarshalStoredAudio(completed[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stored.Path, outputPath); err != nil {
		t.Fatal(err)
	}
	probe := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "default=noprint_wrappers=1:nokey=1", outputPath)
	codec, err := probe.Output()
	if err != nil || strings.TrimSpace(string(codec)) != "opus" {
		t.Fatalf("invalid Ogg/Opus: %q, %v", codec, err)
	}
	t.Logf("English transcript: %s", english)
	t.Logf("Spanish transcript: %s", spanish)
	t.Logf("Spanish Ogg/Opus: %s", outputPath)
}

func waitForSubscriptions(t *testing.T, bus *memoryBus, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		bus.mu.Lock()
		ready := len(bus.handlers) == count
		bus.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("stages did not subscribe")
		}
		time.Sleep(time.Millisecond)
	}
}

func publishSample(t *testing.T, ctx context.Context, bus qol.Publisher, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	buffer := make([]byte, 256*1024)
	var sequence uint64
	for {
		size, readErr := io.ReadFull(file, buffer)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			t.Fatal(readErr)
		}
		end := readErr == io.EOF || readErr == io.ErrUnexpectedEOF
		payload, err := wire.MarshalAudioStream("audio/mpeg", filepath.Base(path), end, buffer[:size])
		if err != nil {
			t.Fatal(err)
		}
		event := qol.Event{ID: string(rune(sequence + 1)), SessionID: "sermon-sample-e2e", Channel: qol.ChannelCaptureInput, Type: qol.TypeAudioStream, Seq: sequence, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload}
		if err := bus.Publish(ctx, event); err != nil {
			t.Fatal(err)
		}
		sequence++
		if end {
			return
		}
	}
}

func textFrom(t *testing.T, bus *memoryBus, channel string) string {
	t.Helper()
	bus.mu.Lock()
	events := append([]qol.Event(nil), bus.events[channel]...)
	bus.mu.Unlock()
	var result strings.Builder
	for _, event := range events {
		text, err := wire.UnmarshalText(event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		result.WriteString(text.Text)
	}
	if result.Len() == 0 {
		t.Fatal("no English transcript")
	}
	return result.String()
}

func translationFrom(t *testing.T, bus *memoryBus) string {
	t.Helper()
	bus.mu.Lock()
	events := append([]qol.Event(nil), bus.events[qol.ChannelTranslationText]...)
	bus.mu.Unlock()
	var result strings.Builder
	for _, event := range events {
		translation, err := wire.UnmarshalTranslation(event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		result.WriteString(translation.Text)
	}
	if result.Len() == 0 {
		t.Fatal("no Spanish translation")
	}
	return result.String()
}
