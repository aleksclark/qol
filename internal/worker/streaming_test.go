package worker_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
	"github.com/aleksclark/qol/internal/worker"
)

type fakeTranslator struct {
	mu       sync.Mutex
	requests []qol.TranslatorRequest
	turns    map[uint64]string
}

func (f *fakeTranslator) Start(ctx context.Context) (io.WriteCloser, io.ReadCloser, func() error, error) {
	return liveFunc(func(input io.Reader, output io.Writer) error {
		reader := bufio.NewReader(input)
		for {
			request, err := wire.ReadTranslatorRequest(reader)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			f.mu.Lock()
			f.requests = append(f.requests, request)
			if f.turns == nil {
				f.turns = map[uint64]string{}
			}
			text := ""
			switch request.Kind {
			case qol.TranslatorTurn:
				f.turns[request.TurnID] = request.Text
				if request.Text != "" && !strings.HasSuffix(request.Text, "...") {
					text = "es:" + request.Text
				}
			case qol.TranslatorCondense:
				var parts []string
				for _, turn := range request.TurnIDs {
					parts = append(parts, f.turns[turn])
				}
				text = fmt.Sprintf("corto(%s)", strings.Join(parts, "+"))
			}
			turns := uint32(len(f.turns))
			f.mu.Unlock()
			if err := wire.WriteTranslatorResponse(output, qol.TranslatorResponse{Text: text, ContextTurns: turns}); err != nil {
				return err
			}
		}
	}).Start(ctx)
}

func (f *fakeTranslator) kinds() []qol.TranslatorRequestKind {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kinds []qol.TranslatorRequestKind
	for _, request := range f.requests {
		kinds = append(kinds, request.Kind)
	}
	return kinds
}

func publishEnglish(t *testing.T, bus *memoryBus, sequence uint64, span qol.Span, text string, final bool) {
	t.Helper()
	payload, err := wire.MarshalText(qol.Text{Text: text, Final: final, Language: "en", Prosody: &qol.Prosody{RelativeRate: 1.1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), qol.Event{ID: fmt.Sprintf("en-%d", sequence), SessionID: "session", Channel: qol.ChannelASRText, Type: qol.TypeTextFinal, Seq: sequence, Span: span, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func publishControl(t *testing.T, bus *memoryBus, sequence uint64, control qol.Control) {
	t.Helper()
	payload, err := wire.MarshalControl(control)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), qol.Event{ID: fmt.Sprintf("control-%d", sequence), SessionID: "session", Channel: qol.ChannelTranslateControl, Type: qol.TypeControl, Seq: sequence, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func translations(t *testing.T, bus *memoryBus) []qol.Translation {
	t.Helper()
	var result []qol.Translation
	for _, event := range bus.channel(qol.ChannelTranslationText) {
		translation, err := wire.UnmarshalTranslation(event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, translation)
	}
	return result
}

func TestStreamingTranslatorCarriesContextAndCondensesOnControl(t *testing.T) {
	llm := &fakeTranslator{}
	bus := newMemoryBus()
	runStages(t, bus, worker.NewStreamingTranslate(llm))
	publishEnglish(t, bus, 0, qol.Span{Start: 0, End: seconds(1)}, "so here is the...", false)
	publishEnglish(t, bus, 1, qol.Span{Start: seconds(1), End: seconds(2)}, "thing", false)
	publishEnglish(t, bus, 2, qol.Span{Start: seconds(2), End: seconds(3)}, "we sell homes", false)
	publishControl(t, bus, 0, qol.Control{Kind: qol.ControlCondense, TargetSequences: []uint64{0, 1}, TargetRatio: 0.6})
	publishControl(t, bus, 1, qol.Control{Kind: qol.ControlCondense, TargetSequences: []uint64{1}, TargetRatio: 0.6})
	publishControl(t, bus, 2, qol.Control{Kind: qol.ControlSummarize})
	publishControl(t, bus, 3, qol.Control{Kind: qol.ControlGlossary, Glossary: []qol.GlossaryEntry{{Source: "Peter", Target: "Pedro"}}})
	publishEnglish(t, bus, 3, qol.Span{Start: seconds(3), End: seconds(3)}, " ", true)

	events := bus.channel(qol.ChannelTranslationText)
	got := translations(t, bus)
	if len(got) != 4 {
		t.Fatalf("expected two translations, one replacement, and an end marker: %#v", got)
	}
	if got[0].Text != "es:thing" || events[0].Span != (qol.Span{Start: 0, End: seconds(2)}) || got[0].Prosody == nil {
		t.Fatalf("held fragments must merge into the next output: %#v %+v", got[0], events[0].Span)
	}
	if got[1].Text != "es:we sell homes" || events[1].Span.Start != seconds(2) {
		t.Fatalf("unexpected second translation: %#v", got[1])
	}
	if got[2].Text != "corto(so here is the...+thing+we sell homes)" || !slices.Equal(got[2].Replaces, []uint64{0, 1}) || events[2].Span != (qol.Span{Start: 0, End: seconds(3)}) {
		t.Fatalf("condense must replace the requested outputs: %#v %+v", got[2], events[2].Span)
	}
	if !got[3].Final {
		t.Fatalf("expected end marker: %#v", got[3])
	}
	want := []qol.TranslatorRequestKind{qol.TranslatorTurn, qol.TranslatorTurn, qol.TranslatorTurn, qol.TranslatorCondense, qol.TranslatorSummarize, qol.TranslatorGlossary, qol.TranslatorEnd}
	if kinds := llm.kinds(); !slices.Equal(kinds, want) {
		t.Fatalf("unexpected translator requests: %v", kinds)
	}
	if telemetry := lastTelemetry(t, bus, "translate"); telemetry.ContextTurns == 0 {
		t.Fatalf("translator must report its context: %+v", telemetry)
	}
}

func publishTelemetry(t *testing.T, bus *memoryBus, sequence uint64, telemetry qol.Telemetry) {
	t.Helper()
	payload, err := wire.MarshalTelemetry(telemetry)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), qol.Event{ID: fmt.Sprintf("%s-%d", telemetry.Stage, sequence), SessionID: "session", Channel: qol.ChannelTelemetry, Type: qol.TypeTelemetry, Seq: sequence, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func controls(t *testing.T, bus *memoryBus) []qol.Control {
	t.Helper()
	var result []qol.Control
	for _, event := range bus.channel(qol.ChannelTranslateControl) {
		control, err := wire.UnmarshalControl(event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, control)
	}
	return result
}

func TestConductorCondensesBacklogAndSummarizesContext(t *testing.T) {
	bus := newMemoryBus()
	runStages(t, bus, worker.NewConductor(worker.ConductorOptions{CondenseLag: 5 * time.Second, SummarizeTurns: 3}))
	publishTelemetry(t, bus, 0, qol.Telemetry{Stage: "tts", QueuedSequences: []uint64{4, 5, 6}, ProjectedLagSeconds: 4})
	publishTelemetry(t, bus, 1, qol.Telemetry{Stage: "tts", QueuedSequences: []uint64{4, 5, 6}, ProjectedLagSeconds: 7})
	publishTelemetry(t, bus, 2, qol.Telemetry{Stage: "tts", QueuedSequences: []uint64{4, 5, 6, 7, 8}, ProjectedLagSeconds: 8})
	publishTelemetry(t, bus, 0, qol.Telemetry{Stage: "translate", ContextTurns: 3})
	publishTelemetry(t, bus, 1, qol.Telemetry{Stage: "translate", ContextTurns: 4})
	publishTelemetry(t, bus, 2, qol.Telemetry{Stage: "translate", ContextTurns: 1})
	publishTelemetry(t, bus, 3, qol.Telemetry{Stage: "translate", ContextTurns: 3})
	got := controls(t, bus)
	if len(got) != 3 {
		t.Fatalf("expected one condense and two summarize directives: %#v", got)
	}
	if got[0].Kind != qol.ControlCondense || !slices.Equal(got[0].TargetSequences, []uint64{6}) || got[0].TargetRatio != worker.DefaultCondenseRatio {
		t.Fatalf("condense must protect the next two items and stay within cooldown: %#v", got[0])
	}
	if got[1].Kind != qol.ControlSummarize || got[2].Kind != qol.ControlSummarize {
		t.Fatalf("summarize must fire once per threshold crossing: %#v", got[1:])
	}
}

func TestControlLoopReplacesBacklogBeforeItIsSpoken(t *testing.T) {
	llm := &fakeTranslator{}
	synth := &synthesizer{}
	bus := newAsyncBus()
	options := worker.DefaultSpeechOptions()
	options.Timeline = worker.Timeline{Delay: time.Second}
	options.Coalesce = false
	runStages(t, bus, worker.NewStreamingTranslate(llm), worker.NewTTS(synth, options), worker.NewConductor(worker.ConductorOptions{CondenseLag: time.Second}))
	publishTelemetry(t, bus, 0, qol.Telemetry{Stage: "prosody", ConsumedThrough: 0})
	for index, text := range []string{"first", "second", "third", "fourth", "fifth"} {
		start := seconds(float64(index) * 2)
		publishEnglish(t, bus, uint64(index), qol.Span{Start: start, End: start + seconds(2)}, text, false)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !slices.ContainsFunc(translations(t, bus), func(translation qol.Translation) bool { return len(translation.Replaces) > 0 }) {
		if time.Now().After(deadline) {
			t.Fatalf("the conductor never condensed the backlog: %#v", controls(t, bus))
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	publishTelemetry(t, bus, 1, qol.Telemetry{Stage: "prosody", ConsumedThrough: seconds(20)})
	publishEnglish(t, bus, 5, qol.Span{Start: seconds(10), End: seconds(10)}, " ", true)
	waitForFinal(t, bus)
	_, requests := synth.snapshot()
	var spoken []string
	for _, request := range requests {
		spoken = append(spoken, request.Text)
	}
	if len(spoken) < 3 || spoken[0] != "es:first" || spoken[1] != "es:second" || !slices.ContainsFunc(spoken, func(text string) bool { return strings.HasPrefix(text, "corto(") }) || slices.Contains(spoken, "es:fifth") && slices.Contains(spoken, "es:third") {
		t.Fatalf("condensed speech must replace queued speech: %q", spoken)
	}
}

func TestStreamingTranslatorFlushesHeldFragmentsAtTheEnd(t *testing.T) {
	llm := &fakeTranslator{}
	bus := newMemoryBus()
	runStages(t, bus, worker.NewStreamingTranslate(llm))
	publishEnglish(t, bus, 0, qol.Span{Start: 0, End: seconds(1)}, "and the rest of the...", false)
	publishEnglish(t, bus, 1, qol.Span{Start: seconds(1), End: seconds(1)}, " ", true)
	llm.mu.Lock()
	requests := append([]qol.TranslatorRequest(nil), llm.requests...)
	llm.mu.Unlock()
	if len(requests) != 3 || requests[1].Kind != qol.TranslatorTurn || !requests[1].Final || requests[2].Kind != qol.TranslatorEnd {
		t.Fatalf("held source must be flushed with a final turn: %#v", requests)
	}
	if got := translations(t, bus); len(got) != 1 || !got[0].Final {
		t.Fatalf("nothing renderable means only the end marker: %#v", got)
	}
}

func TestTTSSpeaksWholeClausesWithNaturalGaps(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	padded := liveFunc(func(input io.Reader, output io.Writer) error {
		reader := bufio.NewReader(input)
		for {
			request, err := wire.ReadSynthesisRequest(reader)
			if err != nil {
				return nil
			}
			mu.Lock()
			texts = append(texts, request.Text)
			mu.Unlock()
			pcm := make([]byte, 2*(2205+4410+2205))
			for index := 2205; index < 2205+4410; index++ {
				pcm[index*2] = 0x10
				pcm[index*2+1] = 0x27
			}
			if err := wire.WriteSynthesisChunk(output, qol.SynthesisChunk{Audio: qol.Audio{Format: speechFormat, Data: pcm}, EndOfUtterance: true}); err != nil {
				return err
			}
		}
	})
	options := worker.DefaultSpeechOptions()
	options.Timeline = worker.Timeline{}
	options.Fade = 0
	options.ClauseHold = time.Second
	options.Gaps = worker.PhraseGaps{Sentence: 300 * time.Millisecond, Clause: 150 * time.Millisecond, Continuation: 50 * time.Millisecond}
	bus := newAsyncBus()
	runStages(t, bus, worker.NewTTS(padded, options))
	clock := func(at qol.MediaTime, sequence uint64) {
		publishTelemetry(t, bus, sequence, qol.Telemetry{Stage: "prosody", ConsumedThrough: at})
		time.Sleep(20 * time.Millisecond)
	}
	spoken := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), texts...)
	}
	clock(seconds(0.5), 0)
	publishTranslation(t, bus, 0, qol.Span{Start: 0, End: seconds(0.1)}, qol.Translation{Text: "uno dos"})
	clock(seconds(1), 1)
	if got := spoken(); len(got) != 0 {
		t.Fatalf("an unfinished clause must wait for its continuation: %q", got)
	}
	publishTranslation(t, bus, 1, qol.Span{Start: seconds(0.1), End: seconds(0.2)}, qol.Translation{Text: "tres."})
	publishTranslation(t, bus, 2, qol.Span{Start: seconds(0.2), End: seconds(0.3)}, qol.Translation{Text: "cuatro,"})
	publishTranslation(t, bus, 3, qol.Span{Start: seconds(0.3), End: seconds(0.4)}, qol.Translation{Text: "cinco"})
	clock(seconds(1.2), 2)
	if got := spoken(); !slices.Equal(got, []string{"uno dos tres.", "cuatro,"}) {
		t.Fatalf("fragments must be spoken as whole clauses: %q", got)
	}
	clock(seconds(2.5), 3)
	publishTranslation(t, bus, 4, qol.Span{Start: seconds(0.4), End: seconds(0.4)}, qol.Translation{Text: " ", Final: true})
	waitForFinal(t, bus)
	if got := spoken(); !slices.Equal(got, []string{"uno dos tres.", "cuatro,", "cinco,"}) {
		t.Fatalf("a clause still open after the hold must be spoken alone: %q", got)
	}
	var units [][]int
	var spans []qol.Span
	for _, event := range bus.channel(qol.ChannelTTSAudio) {
		if event.Type != qol.TypeAudioPCM {
			continue
		}
		audio, err := wire.UnmarshalAudio(event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(spans) == 0 || spans[len(spans)-1] != event.Span {
			spans = append(spans, event.Span)
			units = append(units, nil)
		}
		units[len(units)-1] = append(units[len(units)-1], len(audio.Data))
	}
	trimmed := 2 * (4410 + 2*441)
	want := [][]int{{2 * 4410, trimmed}, {2 * 6615, trimmed}, {2 * 3307, trimmed}}
	if len(units) != 3 || !slices.Equal(units[0], want[0]) || !slices.Equal(units[1], want[1]) || !slices.Equal(units[2], want[2]) {
		t.Fatalf("expected trimmed speech separated by 300 ms after a sentence and 150 ms after a comma: %v", units)
	}
}
