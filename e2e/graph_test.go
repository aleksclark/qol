package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/evaluation"
	"github.com/aleksclark/qol/internal/media"
	"github.com/aleksclark/qol/internal/prosody"
	"github.com/aleksclark/qol/internal/wire"
	"github.com/aleksclark/qol/internal/worker"
)

type memoryBus struct {
	mu            sync.Mutex
	events        map[string][]qol.Event
	subscriptions map[string][]*subscription
}

func newMemoryBus() *memoryBus {
	return &memoryBus{events: make(map[string][]qol.Event), subscriptions: make(map[string][]*subscription)}
}

func (b *memoryBus) Publish(ctx context.Context, event qol.Event) error {
	b.mu.Lock()
	b.events[event.Channel] = append(b.events[event.Channel], event)
	subscriptions := append([]*subscription(nil), b.subscriptions[event.Channel]...)
	b.mu.Unlock()
	for _, subscription := range subscriptions {
		subscription.enqueue(event)
	}
	return ctx.Err()
}

func (b *memoryBus) channel(name string) []qol.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]qol.Event(nil), b.events[name]...)
}

func (b *memoryBus) Subscribe(ctx context.Context, channel string, _ qol.SubscribeOptions, handler qol.Handler) (qol.Subscription, error) {
	result := &subscription{done: make(chan struct{})}
	result.ready = sync.NewCond(&result.mu)
	b.mu.Lock()
	b.subscriptions[channel] = append(b.subscriptions[channel], result)
	b.mu.Unlock()
	go result.deliver(ctx, handler)
	return result, nil
}

type subscription struct {
	mu      sync.Mutex
	ready   *sync.Cond
	pending []qol.Event
	closed  bool
	done    chan struct{}
	once    sync.Once
}

func (s *subscription) enqueue(event qol.Event) {
	s.mu.Lock()
	s.pending = append(s.pending, event)
	s.mu.Unlock()
	s.ready.Signal()
}

func (s *subscription) deliver(ctx context.Context, handler qol.Handler) {
	for {
		s.mu.Lock()
		for len(s.pending) == 0 && !s.closed {
			s.ready.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		event := s.pending[0]
		s.pending = s.pending[1:]
		s.mu.Unlock()
		if err := handler(ctx, event); err != nil {
			log.Printf("handler for %s failed: %v", event.Channel, err)
		}
	}
}

func (s *subscription) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.ready.Broadcast()
		close(s.done)
	})
	return nil
}
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
	python := envOr("QOL_E2E_PYTHON", "/home/aleks/.cache/qol/proc-venv/bin/python")
	ttsPython := envOr("QOL_E2E_TTS_PYTHON", python)
	ttsProcessor := envOr("QOL_E2E_TTS_PROCESSOR", "tts_spanish.py")
	emotionPython := os.Getenv("QOL_E2E_EMOTION_PYTHON")
	outputDir := filepath.Join(root, "e2e-artifacts")
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		t.Fatal(err)
	}
	sample := clipSample(t, envOr("QOL_E2E_SAMPLE", filepath.Join(root, "sermon_sample.mp3")), envOr("QOL_E2E_SECONDS", "90"))
	outputPath := filepath.Join(outputDir, "sermon_sample_spanish.ogg")
	_ = os.Remove(outputPath)
	for _, name := range []string{"sermon-sample-e2e-record-en.ogg", "sermon-sample-e2e-record-es.ogg"} {
		_ = os.Remove(filepath.Join(outputDir, name))
	}
	processors := filepath.Join(root, "e2e/processors")
	stages := []qol.Stage{
		worker.NewCapture(),
		worker.NewSTT(worker.NewCommandProcessor(python, filepath.Join(processors, "stt_whisper.py"))),
		worker.NewProsody(media.NewDecoder("ffmpeg"), emotionStage(emotionPython, processors), prosody.DefaultReferenceRate, 30*time.Second),
		worker.NewTranslate(worker.NewCommandProcessor(python, filepath.Join(processors, "translate_en_es.py"))),
		worker.NewTTS(worker.NewCommandProcessor(ttsPython, filepath.Join(processors, ttsProcessor)), timelineFromEnv(t)),
		worker.NewRecord("record-en", qol.ChannelRecordENInput, qol.ChannelRecordENCompleted, worker.NewFFmpegOggOpusStore("ffmpeg", outputDir)),
		worker.NewRecord("record-es", qol.ChannelTTSAudio, qol.ChannelRecordESCompleted, worker.NewFFmpegS16LEOggOpusStore("ffmpeg", outputDir, worker.SynthesisSampleRate, 1)),
	}
	bus := newMemoryBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputs := 0
	for _, stage := range stages {
		inputs += len(stage.Spec().Inputs)
		go func(stage qol.Stage) { _ = stage.Run(ctx, bus) }(stage)
	}
	waitForSubscriptions(t, bus, inputs)
	started := time.Now()
	publishSample(t, ctx, bus, sample)
	waitForEvents(t, bus, qol.ChannelRecordESCompleted, 1, 10*time.Minute)
	elapsed := time.Since(started)

	english := textFrom(t, bus, qol.ChannelASRText)
	spanish := translationFrom(t, bus)
	if strings.EqualFold(strings.TrimSpace(english), strings.TrimSpace(spanish)) {
		t.Fatalf("translation did not change the transcript: %q", spanish)
	}
	var classify evaluation.Classifier
	if emotionPython != "" {
		classify = emotionClassifier(t, emotionPython, filepath.Join(processors, "emotion_emotion2vec.py"))
	}
	utterances, err := evaluation.FromGraphEvents(bus.channel(qol.ChannelTranslationText), bus.channel(qol.ChannelTTSAudio))
	if err != nil {
		t.Fatal(err)
	}
	report, err := evaluation.CarryThrough(utterances, classify)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range report.Rows {
		t.Log(row)
	}
	if report.Utterances < 5 {
		t.Fatalf("expected at least 5 annotated utterances, got %d", report.Utterances)
	}
	if report.Annotated*10 < report.Utterances*9 {
		t.Fatalf("only %d of %d utterances carried prosody", report.Annotated, report.Utterances)
	}
	t.Logf("wall=%s %s", elapsed.Round(time.Millisecond), report.Summary())
	if dump := os.Getenv("QOL_E2E_DUMP_SYNTHESIS"); dump != "" {
		writeSynthesisFixture(t, dump, utterances)
	}
	for name, value := range map[string]float64{"QOL_E2E_MIN_RATE_CORRELATION": report.Rate, "QOL_E2E_MIN_PITCH_CORRELATION": report.Pitch} {
		if minimum := os.Getenv(name); minimum != "" {
			threshold, err := strconv.ParseFloat(minimum, 64)
			if err != nil {
				t.Fatal(err)
			}
			if value < threshold {
				t.Fatalf("%s: carry-through %.2f is below %.2f", name, value, threshold)
			}
		}
	}
	if maximum := os.Getenv("QOL_E2E_MAX_P90_LAG"); maximum != "" {
		limit, err := time.ParseDuration(maximum)
		if err != nil {
			t.Fatal(err)
		}
		if report.LagP90 > limit {
			t.Fatalf("p90 onset lag %s exceeds %s", report.LagP90, limit)
		}
	}

	completed := bus.channel(qol.ChannelRecordESCompleted)
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

func emotionClassifier(t *testing.T, python, script string) evaluation.Classifier {
	t.Helper()
	input, output, wait, err := worker.NewCommandProcessor(python, script).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		_ = wait()
	})
	reader := bufio.NewReader(output)
	return func(samples []int16, sampleRate int) (qol.EmotionResult, error) {
		raw := make([]byte, len(samples)*2)
		for index, sample := range samples {
			binary.LittleEndian.PutUint16(raw[index*2:], uint16(sample))
		}
		command := exec.Command("ffmpeg", "-v", "error", "-f", "s16le", "-ar", strconv.Itoa(sampleRate), "-ac", "1", "-i", "pipe:0", "-f", "s16le", "-ar", "16000", "-ac", "1", "pipe:1")
		command.Stdin = bytes.NewReader(raw)
		resampled, err := command.Output()
		if err != nil {
			return qol.EmotionResult{}, err
		}
		if err := wire.WriteEmotionRequest(input, qol.Audio{Format: qol.PCMFormat{SampleRate: 16000, Channels: 1, Format: qol.SampleS16LE}, Data: resampled}); err != nil {
			return qol.EmotionResult{}, err
		}
		return wire.ReadEmotionResult(reader)
	}
}

func writeSynthesisFixture(t *testing.T, path string, utterances []evaluation.Utterance) {
	t.Helper()
	var buffer bytes.Buffer
	for _, item := range utterances {
		if err := wire.WriteSynthesisRequest(&buffer, qol.SynthesisRequest{Text: item.Text, Language: item.Language, Prosody: item.Source}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Logf("synthesis fixture: %s (%d requests)", path, len(utterances))
}

func timelineFromEnv(t *testing.T) worker.Timeline {
	t.Helper()
	timeline := worker.DefaultTimeline()
	if value := os.Getenv("QOL_E2E_PLAYOUT_DELAY"); value != "" {
		delay, err := time.ParseDuration(value)
		if err != nil {
			t.Fatal(err)
		}
		timeline.Delay = delay
	}
	if value := os.Getenv("QOL_E2E_MAX_PACE"); value != "" {
		pace, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatal(err)
		}
		timeline.MaxPace = pace
	}
	return timeline
}

func emotionStage(python, processors string) worker.LiveProcessor {
	if python == "" {
		return nil
	}
	return worker.NewCommandProcessor(python, filepath.Join(processors, "emotion_emotion2vec.py"))
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func clipSample(t *testing.T, source, seconds string) string {
	t.Helper()
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("set QOL_E2E_SAMPLE to an English recording: %v", err)
	}
	clipped := filepath.Join(t.TempDir(), "sample.mp3")
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", source, "-t", seconds, "-c", "copy", clipped)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("clip sample: %v: %s", err, output)
	}
	return clipped
}

func waitForEvents(t *testing.T, bus *memoryBus, channel string, count int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for len(bus.channel(channel)) < count {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", channel)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitForSubscriptions(t *testing.T, bus *memoryBus, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		bus.mu.Lock()
		ready := 0
		for _, subscriptions := range bus.subscriptions {
			ready += len(subscriptions)
		}
		bus.mu.Unlock()
		if ready == count {
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
