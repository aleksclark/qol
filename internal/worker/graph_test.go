package worker_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	return &memorySubscription{done: make(chan struct{})}, nil
}

type memorySubscription struct {
	done chan struct{}
	once sync.Once
}

func (s *memorySubscription) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}
func (s *memorySubscription) Done() <-chan struct{} { return s.done }
func (s *memorySubscription) Err() error            { return nil }

type processor func([]byte) []byte

func (p processor) Process(_ context.Context, input io.Reader, output io.Writer) error {
	data, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	_, err = output.Write(p(data))
	return err
}

func (p processor) Start(context.Context) (io.WriteCloser, io.ReadCloser, func() error, error) {
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		defer close(done)
		defer outputWriter.Close()
		defer inputReader.Close()
		buffer := make([]byte, 64)
		emitted := false
		for {
			n, err := inputReader.Read(buffer)
			if n > 0 && !emitted {
				if _, writeErr := outputWriter.Write(p(buffer[:n])); writeErr != nil {
					done <- writeErr
					return
				}
				if _, writeErr := outputWriter.Write([]byte("\n")); writeErr != nil {
					done <- writeErr
					return
				}
				emitted = true
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					done <- err
				}
				return
			}
		}
	}()
	return inputWriter, outputReader, func() error { return <-done }, nil
}

type store struct {
	directory string
	data      []byte
}

type storeWriter struct {
	store     *store
	sessionID string
}

func (s *store) Open(_ context.Context, sessionID string) (worker.StreamAudioWriter, error) {
	return &storeWriter{store: s, sessionID: sessionID}, nil
}

func (w *storeWriter) Write(data []byte) error {
	w.store.data = append(w.store.data, data...)
	return nil
}

func (w *storeWriter) Close() (qol.StoredAudio, error) {
	path := filepath.Join(w.store.directory, w.sessionID+".ogg")
	if err := os.WriteFile(path, w.store.data, 0o640); err != nil {
		return qol.StoredAudio{}, err
	}
	return qol.StoredAudio{MediaType: "audio/ogg; codecs=opus", Path: path, SizeBytes: uint64(len(w.store.data))}, nil
}

func (w *storeWriter) Abort() error { return nil }

func TestStagesConformToGenericArchitecture(t *testing.T) {
	spool := t.TempDir()
	capture := worker.NewCapture()
	stt := worker.NewSTT(processor(func(data []byte) []byte { return []byte("hello") }), spool)
	translate := worker.NewTranslate(processor(func(data []byte) []byte { return []byte("hola") }), spool)
	tts := worker.NewTTS(processor(func(data []byte) []byte { return []byte("pcm:" + string(data)) }), spool)
	english := &store{directory: t.TempDir()}
	spanish := &store{directory: t.TempDir()}
	recordEN := worker.NewRecord("record-en", qol.ChannelRecordENInput, qol.ChannelRecordENCompleted, english, spool)
	recordES := worker.NewRecord("record-es", qol.ChannelTTSAudio, qol.ChannelRecordESCompleted, spanish, spool)

	assertSpec(t, capture.Spec(), "capture", qol.ChannelCaptureInput, qol.ChannelCaptureOutput)
	if len(capture.Spec().Outputs) != 3 || capture.Spec().Outputs[1].Channel != qol.ChannelAudioInput || capture.Spec().Outputs[2].Channel != qol.ChannelRecordENInput {
		t.Fatalf("capture must fan out to STT and record-en: %#v", capture.Spec())
	}
	assertSpec(t, stt.Spec(), "stt-whisper", qol.ChannelAudioInput, qol.ChannelASRText)
	assertSpec(t, translate.Spec(), "translate", qol.ChannelASRText, qol.ChannelTranslationText)
	assertSpec(t, tts.Spec(), "tts", qol.ChannelTranslationText, qol.ChannelTTSAudio)
	assertSpec(t, recordEN.Spec(), "record-en", qol.ChannelRecordENInput, qol.ChannelRecordENCompleted)
	assertSpec(t, recordES.Spec(), "record-es", qol.ChannelTTSAudio, qol.ChannelRecordESCompleted)

	bus := newMemoryBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, stage := range []qol.Stage{capture, stt, translate, tts, recordEN, recordES} {
		go func(stage qol.Stage) {
			_ = stage.Run(ctx, bus)
		}(stage)
	}
	deadline := time.Now().Add(time.Second)
	for {
		bus.mu.Lock()
		ready := len(bus.handlers) == 6
		bus.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stages did not subscribe")
		}
		time.Sleep(time.Millisecond)
	}

	firstPayload, err := wire.MarshalAudioStream("audio/mpeg", "voice.mp3", false, []byte("mp"))
	if err != nil {
		t.Fatal(err)
	}
	lastPayload, err := wire.MarshalAudioStream("audio/mpeg", "voice.mp3", true, []byte("3"))
	if err != nil {
		t.Fatal(err)
	}
	first := qol.Event{ID: "a", SessionID: "session", Channel: qol.ChannelCaptureInput, Type: qol.TypeAudioStream, Seq: 0, Span: qol.Span{End: 1}, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: firstPayload}
	if err := bus.Publish(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		bus.mu.Lock()
		texts := append([]qol.Event(nil), bus.events[qol.ChannelASRText]...)
		translations := append([]qol.Event(nil), bus.events[qol.ChannelTranslationText]...)
		audio := append([]qol.Event(nil), bus.events[qol.ChannelTTSAudio]...)
		bus.mu.Unlock()
		if len(texts) > 0 && len(translations) > 0 && len(audio) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("translate and TTS did not emit before the end chunk")
		}
		time.Sleep(time.Millisecond)
	}
	last := qol.Event{ID: "b", SessionID: "session", Channel: qol.ChannelCaptureInput, Type: qol.TypeAudioStream, Seq: 1, Span: qol.Span{End: 2}, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: lastPayload}
	if err := bus.Publish(context.Background(), last); err != nil {
		t.Fatal(err)
	}
	bus.mu.Lock()
	texts := append([]qol.Event(nil), bus.events[qol.ChannelASRText]...)
	bus.mu.Unlock()
	if len(texts) < 2 {
		t.Fatalf("expected incremental and final STT events, got %#v", texts)
	}

	bus.mu.Lock()
	englishDone := append([]qol.Event(nil), bus.events[qol.ChannelRecordENCompleted]...)
	spanishDone := append([]qol.Event(nil), bus.events[qol.ChannelRecordESCompleted]...)
	bus.mu.Unlock()
	if len(englishDone) != 1 || len(spanishDone) != 1 {
		t.Fatalf("expected both records to complete, got en=%#v es=%#v", englishDone, spanishDone)
	}
	if !bytes.Equal(english.data, []byte("mp3")) {
		t.Fatalf("unexpected english recording: %q", english.data)
	}
	if !bytes.Equal(spanish.data, append([]byte("pcm:hola"), 0, 0)) {
		t.Fatalf("unexpected spanish recording: %q", spanish.data)
	}
}

func assertSpec(t *testing.T, spec qol.StageSpec, name, input, output string) {
	t.Helper()
	if spec.Name != name || len(spec.Inputs) != 1 || spec.Inputs[0].Channel != input || len(spec.Outputs) == 0 || spec.Outputs[0].Channel != output || len(spec.Inputs[0].Types) == 0 || len(spec.Outputs[0].Types) == 0 {
		t.Fatalf("unexpected stage spec: %#v", spec)
	}
}
