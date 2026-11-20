package evaluation_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/evaluation"
	"github.com/aleksclark/qol/internal/wire"
)

const rate = 16000

func tone(duration time.Duration, hz, amplitude float64) []int16 {
	samples := make([]int16, int(duration.Seconds()*rate))
	for index := range samples {
		phase := 2 * math.Pi * hz * float64(index) / rate
		samples[index] = int16(amplitude * 32767 * (math.Sin(phase) + 0.5*math.Sin(2*phase)) / 1.5)
	}
	return samples
}

func TestCarryThroughCorrelatesSourceAndOutput(t *testing.T) {
	var utterances []evaluation.Utterance
	for index, relative := range []float64{0.8, 1.0, 1.2, 1.4} {
		speech := time.Duration(float64(2*time.Second) / relative)
		samples := append(make([]int16, rate/2), tone(speech, 120*math.Pow(2, relative-1), 0.1*relative)...)
		utterances = append(utterances, evaluation.Utterance{
			Span:       qol.Span{Start: qol.MediaTime(time.Duration(index) * 3 * time.Second), End: qol.MediaTime(time.Duration(index)*3*time.Second + 2*time.Second)},
			Text:       "uno dos tres cuatro",
			Language:   "es",
			Source:     &qol.Prosody{Rate: float32(4 * relative), RelativeRate: float32(relative), RelativeEnergyDB: float32(20 * math.Log10(relative)), RelativePitchSemitones: float32(12 * (relative - 1)), Emotion: "neutral"},
			Start:      time.Duration(index)*3*time.Second + 3*time.Second,
			Samples:    samples,
			SampleRate: rate,
		})
	}
	classified := 0
	report, err := evaluation.CarryThrough(utterances, func(samples []int16, sampleRate int) (qol.EmotionResult, error) {
		classified++
		if sampleRate != rate || len(samples) == 0 {
			t.Fatalf("classifier received %d samples at %d Hz", len(samples), sampleRate)
		}
		return qol.EmotionResult{Label: "neutral", Confidence: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Rate < 0.99 || report.Energy < 0.99 || report.Pitch < 0.99 {
		t.Fatalf("perfect transfer must correlate: %s", report.Summary())
	}
	if report.LagMedian < 3400*time.Millisecond || report.LagMedian > 3600*time.Millisecond {
		t.Fatalf("lag is output speech onset minus source start: %s", report.Summary())
	}
	if classified != 4 || report.EmotionMatches != 4 || report.Annotated != 4 {
		t.Fatalf("emotion agreement: %s", report.Summary())
	}
}

func TestFromSynthesisPairsRequestsWithAudio(t *testing.T) {
	var requests, chunks bytes.Buffer
	format := qol.PCMFormat{SampleRate: 22050, Channels: 1, Format: qol.SampleS16LE}
	for index, text := range []string{"hola", "adiós"} {
		if err := wire.WriteSynthesisRequest(&requests, qol.SynthesisRequest{Text: text, Language: "es", Prosody: &qol.Prosody{RelativeRate: 1}}); err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 4)
		binary.LittleEndian.PutUint16(data, uint16(index+1))
		for _, end := range []bool{false, true} {
			if err := wire.WriteSynthesisChunk(&chunks, qol.SynthesisChunk{Audio: qol.Audio{Format: format, Data: data}, EndOfUtterance: end}); err != nil {
				t.Fatal(err)
			}
		}
	}
	utterances, err := evaluation.FromSynthesis(&requests, &chunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(utterances) != 2 || utterances[1].Text != "adiós" || len(utterances[1].Samples) != 4 || utterances[1].Samples[0] != 2 || utterances[0].SampleRate != 22050 || utterances[0].Start != -1 {
		t.Fatalf("unexpected utterances: %+v", utterances)
	}
	if _, err := evaluation.FromSynthesis(bytes.NewReader(nil), bytes.NewReader([]byte{1})); err != nil {
		t.Fatalf("no requests means no audio to read: %v", err)
	}
}

func TestFromGraphEventsTracksOutputPosition(t *testing.T) {
	span := qol.Span{Start: 0, End: qol.MediaTime(time.Second)}
	translation, err := wire.MarshalTranslation(qol.Translation{Text: "hola", SourceLanguage: "en", TargetLanguage: "es"})
	if err != nil {
		t.Fatal(err)
	}
	silence, err := wire.MarshalAudio(qol.Audio{Format: qol.PCMFormat{SampleRate: 1000, Channels: 1, Format: qol.SampleS16LE}, Data: make([]byte, 2000)})
	if err != nil {
		t.Fatal(err)
	}
	events := []qol.Event{{Seq: 1, Type: qol.TypeAudioPCM, Span: span, Payload: silence}, {Seq: 0, Type: qol.TypeAudioPCM, Span: span, Payload: silence}}
	utterances, err := evaluation.FromGraphEvents([]qol.Event{{Span: span, Payload: translation}}, events)
	if err != nil {
		t.Fatal(err)
	}
	if len(utterances) != 1 || utterances[0].Start != 0 || len(utterances[0].Samples) != 2000 {
		t.Fatalf("unexpected utterance: start=%s samples=%d", utterances[0].Start, len(utterances[0].Samples))
	}
}
