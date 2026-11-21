package worker_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
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
	errors   []error
	async    bool
}

func newAsyncBus() *memoryBus {
	bus := newMemoryBus()
	bus.async = true
	return bus
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
			b.mu.Lock()
			b.errors = append(b.errors, err)
			b.mu.Unlock()
			return err
		}
	}
	return nil
}

func (b *memoryBus) Subscribe(ctx context.Context, channel string, _ qol.SubscribeOptions, handler qol.Handler) (qol.Subscription, error) {
	if b.async {
		handler = b.queued(ctx, handler)
	}
	b.mu.Lock()
	b.handlers[channel] = append(b.handlers[channel], handler)
	b.mu.Unlock()
	return &memorySubscription{done: make(chan struct{})}, nil
}

func (b *memoryBus) queued(ctx context.Context, handler qol.Handler) qol.Handler {
	var mu sync.Mutex
	var pending []qol.Event
	ready := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ready:
			}
			for {
				mu.Lock()
				if len(pending) == 0 {
					mu.Unlock()
					break
				}
				event := pending[0]
				pending = pending[1:]
				mu.Unlock()
				if err := handler(ctx, event); err != nil {
					b.mu.Lock()
					b.errors = append(b.errors, err)
					b.mu.Unlock()
				}
			}
		}
	}()
	return func(_ context.Context, event qol.Event) error {
		mu.Lock()
		pending = append(pending, event)
		mu.Unlock()
		select {
		case ready <- struct{}{}:
		default:
		}
		return nil
	}
}

func (b *memoryBus) channel(name string) []qol.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]qol.Event(nil), b.events[name]...)
}

func (b *memoryBus) waitFor(t *testing.T, name string, count int) []qol.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		events := b.channel(name)
		if len(events) >= count {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d events on %s, got %d (errors %v)", count, name, len(events), b.errors)
		}
		time.Sleep(time.Millisecond)
	}
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

type liveFunc func(io.Reader, io.Writer) error

func (f liveFunc) Start(context.Context) (io.WriteCloser, io.ReadCloser, func() error, error) {
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := f(inputReader, outputWriter)
		_ = inputReader.CloseWithError(errors.New("processor exited"))
		_ = outputWriter.CloseWithError(err)
		done <- err
	}()
	return inputWriter, outputReader, func() error { return <-done }, nil
}

func passthroughDecoder() liveFunc {
	return func(input io.Reader, output io.Writer) error {
		_, err := io.Copy(output, input)
		return err
	}
}

func lineTranslator(translate func(string) string) liveFunc {
	return func(input io.Reader, output io.Writer) error {
		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			if _, err := io.WriteString(output, translate(scanner.Text())+"\n"); err != nil {
				return err
			}
		}
		return scanner.Err()
	}
}

type synthesizer struct {
	mu       sync.Mutex
	requests []qol.SynthesisRequest
	starts   int
}

func (s *synthesizer) Start(ctx context.Context) (io.WriteCloser, io.ReadCloser, func() error, error) {
	s.mu.Lock()
	s.starts++
	s.mu.Unlock()
	return liveFunc(func(input io.Reader, output io.Writer) error {
		reader := bufio.NewReader(input)
		for {
			request, err := wire.ReadSynthesisRequest(reader)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			s.mu.Lock()
			s.requests = append(s.requests, request)
			s.mu.Unlock()
			format := qol.PCMFormat{SampleRate: worker.SynthesisSampleRate, Channels: 1, Format: qol.SampleS16LE}
			if err := wire.WriteSynthesisChunk(output, qol.SynthesisChunk{Audio: qol.Audio{Format: format, Data: []byte("pcm:")}}); err != nil {
				return err
			}
			if err := wire.WriteSynthesisChunk(output, qol.SynthesisChunk{Audio: qol.Audio{Format: format, Data: []byte(request.Text)}, EndOfUtterance: true}); err != nil {
				return err
			}
		}
	}).Start(ctx)
}

func voicePCM(duration time.Duration, hz, amplitude float64) []byte {
	count := int(duration.Seconds() * worker.ProsodySampleRate)
	data := make([]byte, count*2)
	for index := range count {
		phase := 2 * math.Pi * hz * float64(index) / worker.ProsodySampleRate
		value := amplitude * (math.Sin(phase) + 0.5*math.Sin(2*phase)) / 1.5
		binary.LittleEndian.PutUint16(data[index*2:], uint16(int16(value*32767)))
	}
	return data
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

func audioEvent(t *testing.T, channel string, sequence uint64, data []byte, end bool) qol.Event {
	t.Helper()
	payload, err := wire.MarshalAudioStream("audio/L16", "voice.pcm", end, data)
	if err != nil {
		t.Fatal(err)
	}
	return qol.Event{ID: "audio-" + string(rune('a'+sequence)), SessionID: "session", Channel: channel, Type: qol.TypeAudioStream, Seq: sequence, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload}
}

func textEvent(t *testing.T, sequence uint64, span qol.Span, text string, final bool) qol.Event {
	t.Helper()
	payload, err := wire.MarshalText(qol.Text{Text: text, Final: final, Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	return qol.Event{ID: "text-" + string(rune('a'+sequence)), SessionID: "session", Channel: qol.ChannelUtterances, Type: qol.TypeTextFinal, Seq: sequence, Span: span, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload}
}

func runStages(t *testing.T, bus *memoryBus, stages ...qol.Stage) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	want := 0
	for _, stage := range stages {
		want += len(stage.Spec().Inputs)
		go func(stage qol.Stage) { _ = stage.Run(ctx, bus) }(stage)
	}
	deadline := time.Now().Add(time.Second)
	for {
		bus.mu.Lock()
		count := 0
		for _, handlers := range bus.handlers {
			count += len(handlers)
		}
		bus.mu.Unlock()
		if count == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("stages did not subscribe")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStagesCarryProsodyThroughTheGraph(t *testing.T) {
	utteranceSpan := qol.Span{Start: 0, End: qol.MediaTime(400 * time.Millisecond)}
	stt := worker.NewSTT(liveFunc(func(input io.Reader, output io.Writer) error {
		buffer := make([]byte, 4096)
		emitted := false
		for {
			_, err := input.Read(buffer)
			if !emitted {
				if writeErr := wire.WriteUtterance(output, qol.Utterance{Span: utteranceSpan, Text: "hello there"}); writeErr != nil {
					return writeErr
				}
				emitted = true
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}))
	synth := &synthesizer{}
	capture := worker.NewCapture()
	prosodyStage := worker.NewProsody(passthroughDecoder(), nil, 4, time.Second)
	translate := worker.NewTranslate(lineTranslator(func(string) string { return "hola" }))
	tts := worker.NewTTS(synth, worker.SpeechOptions{})
	english := &store{directory: t.TempDir()}
	spanish := &store{directory: t.TempDir()}
	recordEN := worker.NewRecord("record-en", qol.ChannelRecordENInput, qol.ChannelRecordENCompleted, english)
	recordES := worker.NewRecord("record-es", qol.ChannelTTSAudio, qol.ChannelRecordESCompleted, spanish)

	assertSpec(t, capture.Spec(), "capture", qol.ChannelCaptureInput, qol.ChannelCaptureOutput)
	outputs := capture.Spec().Outputs
	if len(outputs) != 4 || outputs[1].Channel != qol.ChannelAudioInput || outputs[2].Channel != qol.ChannelRecordENInput || outputs[3].Channel != qol.ChannelProsodyAudio {
		t.Fatalf("capture must fan out to STT, record-en, and prosody: %#v", capture.Spec())
	}
	assertSpec(t, stt.Spec(), "stt-whisper", qol.ChannelAudioInput, qol.ChannelUtterances)
	assertSpec(t, prosodyStage.Spec(), "prosody", qol.ChannelUtterances, qol.ChannelASRText)
	if prosodyStage.Spec().Inputs[1].Channel != qol.ChannelProsodyAudio {
		t.Fatalf("prosody must read source audio: %#v", prosodyStage.Spec())
	}
	assertSpec(t, translate.Spec(), "translate", qol.ChannelASRText, qol.ChannelTranslationText)
	assertSpec(t, tts.Spec(), "tts", qol.ChannelTranslationText, qol.ChannelTTSAudio)
	assertSpec(t, recordEN.Spec(), "record-en", qol.ChannelRecordENInput, qol.ChannelRecordENCompleted)
	assertSpec(t, recordES.Spec(), "record-es", qol.ChannelTTSAudio, qol.ChannelRecordESCompleted)

	bus := newMemoryBus()
	runStages(t, bus, capture, stt, prosodyStage, translate, tts, recordEN, recordES)

	source := voicePCM(500*time.Millisecond, 150, 0.3)
	first := audioEvent(t, qol.ChannelCaptureInput, 0, source, false)
	if err := bus.Publish(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	audio := bus.waitFor(t, qol.ChannelTTSAudio, 2)
	if audio[0].Span != utteranceSpan || audio[0].Type != qol.TypeAudioPCM {
		t.Fatalf("TTS audio must keep the utterance span before the stream ends: %#v", audio[0])
	}
	last := audioEvent(t, qol.ChannelCaptureInput, 1, []byte{0, 0}, true)
	if err := bus.Publish(context.Background(), last); err != nil {
		t.Fatal(err)
	}

	utterances := bus.channel(qol.ChannelUtterances)
	if len(utterances) != 2 || utterances[0].Span != utteranceSpan {
		t.Fatalf("unexpected STT utterances: %#v", utterances)
	}
	annotated := bus.channel(qol.ChannelASRText)
	if len(annotated) != 2 {
		t.Fatalf("expected annotated text and end marker, got %#v", annotated)
	}
	text, err := wire.UnmarshalText(annotated[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if text.Prosody == nil || math.Abs(float64(text.Prosody.PitchHz)-150) > 4 || text.Prosody.Rate <= 0 || annotated[0].Span != utteranceSpan || annotated[0].Parents[0] != utterances[0].ID {
		t.Fatalf("prosody stage must measure the source utterance: %#v %#v", annotated[0], text.Prosody)
	}
	translations := bus.channel(qol.ChannelTranslationText)
	translation, err := wire.UnmarshalTranslation(translations[0].Payload)
	if err != nil || translation.Text != "hola" || translation.Prosody == nil || translation.Prosody.PitchHz != text.Prosody.PitchHz || translations[0].Span != utteranceSpan {
		t.Fatalf("translation must carry prosody and span: %#v %v", translation, err)
	}
	synth.mu.Lock()
	requests := append([]qol.SynthesisRequest(nil), synth.requests...)
	synth.mu.Unlock()
	if len(requests) != 1 || requests[0].Language != "es" || requests[0].Prosody == nil || requests[0].Prosody.Rate != text.Prosody.Rate {
		t.Fatalf("synthesizer must receive prosody: %#v", requests)
	}
	englishDone := bus.channel(qol.ChannelRecordENCompleted)
	spanishDone := bus.channel(qol.ChannelRecordESCompleted)
	if len(englishDone) != 1 || len(spanishDone) != 1 {
		t.Fatalf("expected both records to complete, got en=%#v es=%#v", englishDone, spanishDone)
	}
	if !bytes.Equal(english.data, append(append([]byte(nil), source...), 0, 0)) {
		t.Fatalf("unexpected english recording length %d", len(english.data))
	}
	leading := make([]byte, 2*worker.SynthesisSampleRate*400/1000)
	if !bytes.Equal(spanish.data, append(append(leading, []byte("pcm:hola")...), 0, 0)) {
		t.Fatalf("Spanish speech must start where the source utterance ends: %d bytes", len(spanish.data))
	}
}

func TestProsodyMeasuresPausesAndForwardsWithoutAudio(t *testing.T) {
	bus := newMemoryBus()
	stage := worker.NewProsody(passthroughDecoder(), nil, 4, 50*time.Millisecond)
	runStages(t, bus, stage)

	early := textEvent(t, 0, qol.Span{Start: 0, End: qol.MediaTime(time.Second)}, "too early", false)
	if err := bus.Publish(context.Background(), early); err != nil {
		t.Fatal(err)
	}
	forwarded := bus.waitFor(t, qol.ChannelASRText, 1)
	text, err := wire.UnmarshalText(forwarded[0].Payload)
	if err != nil || text.Prosody != nil || text.Text != "too early" {
		t.Fatalf("text without audio must be forwarded without prosody: %#v %v", text, err)
	}

	quiet := voicePCM(time.Second, 140, 0.1)
	loud := voicePCM(time.Second, 200, 0.4)
	gap := make([]byte, 2*worker.ProsodySampleRate)
	pcm := append(append(append(append([]byte(nil), make([]byte, 2*worker.ProsodySampleRate)...), quiet...), gap...), loud...)
	if err := bus.Publish(context.Background(), audioEvent(t, qol.ChannelProsodyAudio, 0, pcm, true)); err != nil {
		t.Fatal(err)
	}
	second := textEvent(t, 1, qol.Span{Start: qol.MediaTime(time.Second), End: qol.MediaTime(2 * time.Second)}, "good morning everyone", false)
	third := textEvent(t, 2, qol.Span{Start: qol.MediaTime(2 * time.Second), End: qol.MediaTime(4 * time.Second)}, "rejoice", false)
	final := textEvent(t, 3, qol.Span{Start: qol.MediaTime(4 * time.Second), End: qol.MediaTime(4 * time.Second)}, " ", true)
	for _, event := range []qol.Event{second, second, third, final} {
		if err := bus.Publish(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	forwarded = bus.waitFor(t, qol.ChannelASRText, 4)
	if len(forwarded) != 4 {
		t.Fatalf("duplicates must be skipped: %d events", len(forwarded))
	}
	baseline, err := wire.UnmarshalText(forwarded[1].Payload)
	if err != nil || baseline.Prosody == nil {
		t.Fatalf("missing baseline prosody: %#v %v", baseline, err)
	}
	excited, err := wire.UnmarshalText(forwarded[2].Payload)
	if err != nil || excited.Prosody == nil {
		t.Fatalf("missing excited prosody: %#v %v", excited, err)
	}
	if pause := excited.Prosody.PauseBeforeMillis; pause < 950 || pause > 1050 {
		t.Fatalf("pause before = %dms, want about 1000ms", pause)
	}
	for _, marker := range []string{"loud", "high", "long-pause"} {
		if !slices.Contains(excited.Prosody.Markers, marker) {
			t.Fatalf("missing %q in %v", marker, excited.Prosody.Markers)
		}
	}
	if excited.Prosody.Arousal <= baseline.Prosody.Arousal {
		t.Fatalf("louder, higher speech must raise arousal: %.2f <= %.2f", excited.Prosody.Arousal, baseline.Prosody.Arousal)
	}
	if clock := lastTelemetry(t, bus, "prosody"); clock.ConsumedThrough != seconds(4) {
		t.Fatalf("prosody must publish the decoded source clock: %+v", clock)
	}
	end, err := wire.UnmarshalText(forwarded[3].Payload)
	if err != nil || !end.Final || end.Prosody != nil || forwarded[3].Seq != 3 {
		t.Fatalf("end marker must pass through: %#v %v", end, err)
	}
}

func TestTranslateRestartsAFailedProcessor(t *testing.T) {
	var mu sync.Mutex
	starts := 0
	translator := liveFunc(func(input io.Reader, output io.Writer) error {
		mu.Lock()
		starts++
		attempt := starts
		mu.Unlock()
		if attempt == 1 {
			return errors.New("model crashed")
		}
		return lineTranslator(func(text string) string { return "es:" + text })(input, output)
	})
	bus := newMemoryBus()
	runStages(t, bus, worker.NewTranslate(translator))
	publish := func(sequence uint64, text string, final bool) error {
		payload, err := wire.MarshalText(qol.Text{Text: text, Final: final, Language: "en"})
		if err != nil {
			t.Fatal(err)
		}
		return bus.Publish(context.Background(), qol.Event{ID: text, SessionID: "session", Channel: qol.ChannelASRText, Type: qol.TypeTextFinal, Seq: sequence, Span: qol.Span{Start: qol.MediaTime(sequence), End: qol.MediaTime(sequence + 1)}, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload})
	}
	if err := publish(0, "first", false); err == nil {
		t.Fatal("expected the crashed processor to fail the event")
	}
	if err := publish(1, "second", false); err != nil {
		t.Fatal(err)
	}
	if err := publish(2, " ", true); err != nil {
		t.Fatal(err)
	}
	events := bus.channel(qol.ChannelTranslationText)
	if len(events) != 2 || events[0].Seq != 0 || events[1].Seq != 1 || events[0].Span.Start != 1 {
		t.Fatalf("unexpected translations: %#v", events)
	}
	translation, err := wire.UnmarshalTranslation(events[0].Payload)
	if err != nil || translation.Text != "es:second" {
		t.Fatalf("unexpected translation: %#v %v", translation, err)
	}
	final, err := wire.UnmarshalTranslation(events[1].Payload)
	if err != nil || !final.Final {
		t.Fatalf("expected end marker: %#v %v", final, err)
	}
}

func publishTranslation(t *testing.T, bus *memoryBus, sequence uint64, span qol.Span, translation qol.Translation) {
	t.Helper()
	translation.SourceLanguage, translation.TargetLanguage = "en", "es"
	payload, err := wire.MarshalTranslation(translation)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), qol.Event{ID: fmt.Sprintf("es-%d", sequence), SessionID: "session", Channel: qol.ChannelTranslationText, Type: qol.TypeTranslationFinal, Seq: sequence, Span: span, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func (s *synthesizer) snapshot() (int, []qol.SynthesisRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts, append([]qol.SynthesisRequest(nil), s.requests...)
}

func TestTTSUsesOneProcessAndSkipsWrongFormats(t *testing.T) {
	synth := &synthesizer{}
	bus := newMemoryBus()
	runStages(t, bus, worker.NewTTS(synth, worker.SpeechOptions{}))
	publishTranslation(t, bus, 0, qol.Span{}, qol.Translation{Text: "uno"})
	publishTranslation(t, bus, 1, qol.Span{}, qol.Translation{Text: "dos"})
	bus.waitFor(t, qol.ChannelTTSAudio, 4)
	starts, requests := synth.snapshot()
	if starts != 1 || len(requests) != 2 || requests[0].Pace != 1 || requests[1].Pace != 1 {
		t.Fatalf("TTS must reuse one processor: starts=%d requests=%d", starts, len(requests))
	}

	wrong := liveFunc(func(input io.Reader, output io.Writer) error {
		reader := bufio.NewReader(input)
		for {
			if _, err := wire.ReadSynthesisRequest(reader); err != nil {
				return nil
			}
			if err := wire.WriteSynthesisChunk(output, qol.SynthesisChunk{Audio: qol.Audio{Format: qol.PCMFormat{SampleRate: 24000, Channels: 1, Format: qol.SampleS16LE}, Data: []byte{1, 2}}, EndOfUtterance: true}); err != nil {
				return err
			}
		}
	})
	other := newMemoryBus()
	runStages(t, other, worker.NewTTS(wrong, worker.SpeechOptions{}))
	publishTranslation(t, other, 0, qol.Span{}, qol.Translation{Text: "tres"})
	publishTranslation(t, other, 1, qol.Span{}, qol.Translation{Text: " ", Final: true})
	events := other.waitFor(t, qol.ChannelTTSAudio, 1)
	if len(events) != 1 || events[0].Type != qol.TypeAudioPCMFinal {
		t.Fatalf("a 24 kHz synthesizer must not reach the recording: %#v", events)
	}
}

func TestTTSSchedulesUtterancesOnTheSourceTimeline(t *testing.T) {
	var mu sync.Mutex
	var paces []float32
	threeSeconds := liveFunc(func(input io.Reader, output io.Writer) error {
		reader := bufio.NewReader(input)
		for {
			request, err := wire.ReadSynthesisRequest(reader)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			mu.Lock()
			paces = append(paces, request.Pace)
			mu.Unlock()
			pcm := make([]byte, 3*2*worker.SynthesisSampleRate)
			for offset := 0; offset < len(pcm); offset += 64 * 1024 {
				end := min(offset+64*1024, len(pcm))
				if err := wire.WriteSynthesisChunk(output, qol.SynthesisChunk{Audio: qol.Audio{Format: speechFormat, Data: pcm[offset:end]}, EndOfUtterance: end == len(pcm)}); err != nil {
					return err
				}
			}
		}
	})
	bus := newMemoryBus()
	runStages(t, bus, worker.NewTTS(threeSeconds, worker.SpeechOptions{Timeline: worker.Timeline{Delay: time.Second, MaxPace: 1.5, CatchUpWindow: 2 * time.Second}}))
	spans := []qol.Span{{Start: 0, End: seconds(2)}, {Start: seconds(2), End: seconds(4)}, {Start: seconds(10), End: seconds(12)}}
	for sequence, span := range spans {
		publishTranslation(t, bus, uint64(sequence), span, qol.Translation{Text: "hola"})
	}
	publishTranslation(t, bus, 3, qol.Span{Start: seconds(12), End: seconds(12)}, qol.Translation{Text: " ", Final: true})
	waitForFinal(t, bus)
	mu.Lock()
	got := append([]float32(nil), paces...)
	mu.Unlock()
	if !slices.Equal(got, []float32{1, 1.25, 1}) {
		t.Fatalf("pace must rise only while output is behind: %v", got)
	}
	bytesBySpan := map[qol.Span]int{}
	for _, event := range bus.channel(qol.ChannelTTSAudio) {
		audio, err := wire.UnmarshalAudio(event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		bytesBySpan[event.Span] += len(audio.Data)
	}
	perSecond := 2 * worker.SynthesisSampleRate
	for index, want := range []int{(3 + 3) * perSecond, 3 * perSecond, (4 + 3) * perSecond} {
		if got := bytesBySpan[spans[index]]; got != want {
			t.Fatalf("utterance %d: %d bytes of silence plus speech, want %d", index, got, want)
		}
	}
}

func TestTTSHoldsSpeechUntilDueAndAppliesReplacements(t *testing.T) {
	synth := &synthesizer{}
	bus := newMemoryBus()
	options := worker.DefaultSpeechOptions()
	options.Timeline = worker.Timeline{Delay: time.Second}
	options.Coalesce = false
	runStages(t, bus, worker.NewTTS(synth, options))
	clock := func(at qol.MediaTime, sequence uint64) {
		payload, err := wire.MarshalTelemetry(qol.Telemetry{Stage: "prosody", ConsumedThrough: at})
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Publish(context.Background(), qol.Event{ID: fmt.Sprintf("clock-%d", sequence), SessionID: "session", Channel: qol.ChannelTelemetry, Type: qol.TypeTelemetry, Seq: sequence, Span: qol.Span{Start: at, End: at}, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	clock(0, 0)
	publishTranslation(t, bus, 0, qol.Span{Start: seconds(10), End: seconds(12)}, qol.Translation{Text: "primero"})
	publishTranslation(t, bus, 1, qol.Span{Start: seconds(12), End: seconds(14)}, qol.Translation{Text: "segundo"})
	publishTranslation(t, bus, 2, qol.Span{Start: seconds(14), End: seconds(16)}, qol.Translation{Text: "tercero"})
	time.Sleep(50 * time.Millisecond)
	if _, requests := synth.snapshot(); len(requests) != 0 {
		t.Fatalf("speech due at 13 s must wait for the clock, got %v", requests)
	}
	telemetry := lastTelemetry(t, bus, "tts")
	if !slices.Equal(telemetry.QueuedSequences, []uint64{0, 1, 2}) || telemetry.BacklogSeconds <= 0 || telemetry.ProjectedLagSeconds < 2 {
		t.Fatalf("TTS must report its revisable backlog: %+v", telemetry)
	}
	publishTranslation(t, bus, 3, qol.Span{Start: seconds(12), End: seconds(16)}, qol.Translation{Text: "resumen", Replaces: []uint64{1, 2}})
	publishTranslation(t, bus, 4, qol.Span{Start: seconds(16), End: seconds(16)}, qol.Translation{Text: "tarde", Replaces: []uint64{0, 9}})
	clock(seconds(11.5), 1)
	waitForRequests(t, synth, 1)
	publishTranslation(t, bus, 5, qol.Span{Start: seconds(16), End: seconds(16)}, qol.Translation{Text: " ", Final: true})
	waitForFinal(t, bus)
	_, requests := synth.snapshot()
	var texts []string
	for _, request := range requests {
		texts = append(texts, request.Text)
	}
	if !slices.Equal(texts, []string{"primero", "resumen"}) {
		t.Fatalf("queued speech must be replaced and stale replacements dropped: %v", texts)
	}
}

func TestTTSFadesAndSmoothsDelivery(t *testing.T) {
	var mu sync.Mutex
	var rates []float32
	loud := liveFunc(func(input io.Reader, output io.Writer) error {
		reader := bufio.NewReader(input)
		for {
			request, err := wire.ReadSynthesisRequest(reader)
			if err != nil {
				return nil
			}
			mu.Lock()
			rates = append(rates, request.Prosody.RelativeRate)
			mu.Unlock()
			pcm := make([]byte, 2*2205)
			for index := 0; index < len(pcm); index += 2 {
				binary.LittleEndian.PutUint16(pcm[index:], uint16(int16(10000)))
			}
			if err := wire.WriteSynthesisChunk(output, qol.SynthesisChunk{Audio: qol.Audio{Format: speechFormat, Data: pcm}, EndOfUtterance: true}); err != nil {
				return err
			}
		}
	})
	bus := newMemoryBus()
	runStages(t, bus, worker.NewTTS(loud, worker.SpeechOptions{Fade: 10 * time.Millisecond, Smoothing: 0.5}))
	publishTranslation(t, bus, 0, qol.Span{}, qol.Translation{Text: "uno", Prosody: &qol.Prosody{RelativeRate: 1}})
	publishTranslation(t, bus, 1, qol.Span{}, qol.Translation{Text: "dos", Prosody: &qol.Prosody{RelativeRate: 2}})
	publishTranslation(t, bus, 2, qol.Span{}, qol.Translation{Text: " ", Final: true})
	waitForFinal(t, bus)
	mu.Lock()
	got := append([]float32(nil), rates...)
	mu.Unlock()
	if !slices.Equal(got, []float32{1, 1.5}) {
		t.Fatalf("delivery changes must be smoothed: %v", got)
	}
	audio, err := wire.UnmarshalAudio(bus.channel(qol.ChannelTTSAudio)[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	samples := len(audio.Data) / 2
	first := int16(binary.LittleEndian.Uint16(audio.Data))
	middle := int16(binary.LittleEndian.Uint16(audio.Data[(samples/2)*2:]))
	last := int16(binary.LittleEndian.Uint16(audio.Data[len(audio.Data)-2:]))
	if first != 0 || middle != 10000 || last > 500 {
		t.Fatalf("speech must fade in and out: first=%d middle=%d last=%d", first, middle, last)
	}
}

var speechFormat = qol.PCMFormat{SampleRate: worker.SynthesisSampleRate, Channels: 1, Format: qol.SampleS16LE}

func seconds(value float64) qol.MediaTime {
	return qol.MediaTime(value * float64(time.Second))
}

func waitForFinal(t *testing.T, bus *memoryBus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, event := range bus.channel(qol.ChannelTTSAudio) {
			if event.Type == qol.TypeAudioPCMFinal {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("TTS did not finish the stream")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForRequests(t *testing.T, synth *synthesizer, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, requests := synth.snapshot(); len(requests) >= count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d synthesis requests", count)
		}
		time.Sleep(time.Millisecond)
	}
}

func lastTelemetry(t *testing.T, bus *memoryBus, stage string) qol.Telemetry {
	t.Helper()
	events := bus.channel(qol.ChannelTelemetry)
	for index := len(events) - 1; index >= 0; index-- {
		telemetry, err := wire.UnmarshalTelemetry(events[index].Payload)
		if err != nil {
			t.Fatal(err)
		}
		if telemetry.Stage == stage {
			return telemetry
		}
	}
	t.Fatalf("no %s telemetry", stage)
	return qol.Telemetry{}
}

func assertSpec(t *testing.T, spec qol.StageSpec, name, input, output string) {
	t.Helper()
	if spec.Name != name || len(spec.Inputs) == 0 || spec.Inputs[0].Channel != input || len(spec.Outputs) == 0 || spec.Outputs[0].Channel != output || len(spec.Inputs[0].Types) == 0 || len(spec.Outputs[0].Types) == 0 {
		t.Fatalf("unexpected stage spec: %#v", spec)
	}
}

func TestProsodyAddsClassifiedEmotion(t *testing.T) {
	var mu sync.Mutex
	var sampleCounts []int
	classifier := liveFunc(func(input io.Reader, output io.Writer) error {
		reader := bufio.NewReader(input)
		for {
			audio, err := wire.ReadEmotionRequest(reader)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			mu.Lock()
			sampleCounts = append(sampleCounts, len(audio.Data)/2)
			mu.Unlock()
			if err := wire.WriteEmotionResult(output, qol.EmotionResult{Label: "happy", Confidence: 0.9, Scores: []qol.EmotionScore{{Label: "happy", Score: 0.9}, {Label: "neutral", Score: 0.1}}}); err != nil {
				return err
			}
		}
	})
	bus := newMemoryBus()
	runStages(t, bus, worker.NewProsody(passthroughDecoder(), classifier, 4, time.Second).AttachReferenceAudio(true))
	if err := bus.Publish(context.Background(), audioEvent(t, qol.ChannelProsodyAudio, 0, voicePCM(time.Second, 160, 0.3), true)); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), textEvent(t, 0, qol.Span{End: qol.MediaTime(time.Second)}, "praise the lord", false)); err != nil {
		t.Fatal(err)
	}
	text, err := wire.UnmarshalText(bus.waitFor(t, qol.ChannelASRText, 1)[0].Payload)
	if err != nil || text.Prosody == nil {
		t.Fatalf("missing prosody: %#v %v", text, err)
	}
	if text.Prosody.Emotion != "happy" || text.Prosody.EmotionConfidence != 0.9 || len(text.Prosody.EmotionScores) != 2 || !slices.Contains(text.Prosody.Markers, "happy") {
		t.Fatalf("emotion not attached: %+v", text.Prosody)
	}
	if reference := text.Prosody.ReferenceAudio; reference == nil || len(reference.Data) != 2*worker.ProsodySampleRate || reference.Format.SampleRate != worker.ProsodySampleRate {
		t.Fatalf("reference audio must carry the source utterance: %+v", reference)
	}
	if len(sampleCounts) != 1 || sampleCounts[0] != worker.ProsodySampleRate {
		t.Fatalf("classifier must receive the utterance audio: %v", sampleCounts)
	}
}

func TestProsodyForwardsWhenTheClassifierFails(t *testing.T) {
	broken := liveFunc(func(io.Reader, io.Writer) error { return errors.New("model missing") })
	bus := newMemoryBus()
	runStages(t, bus, worker.NewProsody(passthroughDecoder(), broken, 4, time.Second))
	if err := bus.Publish(context.Background(), audioEvent(t, qol.ChannelProsodyAudio, 0, voicePCM(time.Second, 160, 0.3), true)); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), textEvent(t, 0, qol.Span{End: qol.MediaTime(time.Second)}, "amen", false)); err != nil {
		t.Fatal(err)
	}
	text, err := wire.UnmarshalText(bus.waitFor(t, qol.ChannelASRText, 1)[0].Payload)
	if err != nil || text.Prosody == nil || text.Prosody.Emotion != "" || text.Prosody.PitchHz == 0 {
		t.Fatalf("prosody must survive a classifier failure without inventing emotion: %#v %v", text.Prosody, err)
	}
}
