package wire_test

import (
	"bufio"
	"bytes"
	"math"
	"reflect"
	"testing"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

func sampleProsody() *qol.Prosody {
	return &qol.Prosody{
		PitchHz:                142.5,
		Energy:                 -21.5,
		Rate:                   5.25,
		Emphasis:               0.4,
		PitchRangeSemitones:    7.5,
		RelativePitchSemitones: 1.5,
		RelativeEnergyDB:       3,
		RelativeRate:           1.25,
		Arousal:                0.7,
		PauseBeforeMillis:      640,
		VoicedRatio:            0.6,
		SpeechSeconds:          2.5,
		Emotion:                "happy",
		EmotionConfidence:      0.8,
		Markers:                []string{"fast", "loud"},
	}
}

func TestProsodyRidesOnTextAndTranslation(t *testing.T) {
	text := qol.Text{Text: "hello", Final: true, Language: "en", Prosody: sampleProsody()}
	payload, err := wire.MarshalText(text)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := wire.UnmarshalText(payload)
	if err != nil || !reflect.DeepEqual(decoded, text) {
		t.Fatalf("unexpected text: %#v, %v", decoded, err)
	}
	translation := qol.Translation{Text: "hola", SourceLanguage: "en", TargetLanguage: "es", Prosody: sampleProsody()}
	payload, err = wire.MarshalTranslation(translation)
	if err != nil {
		t.Fatal(err)
	}
	decodedTranslation, err := wire.UnmarshalTranslation(payload)
	if err != nil || !reflect.DeepEqual(decodedTranslation, translation) {
		t.Fatalf("unexpected translation: %#v, %v", decodedTranslation, err)
	}
	plain, err := wire.MarshalText(qol.Text{Text: "hello", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = wire.UnmarshalText(plain)
	if err != nil || decoded.Prosody != nil {
		t.Fatalf("absent prosody must stay absent: %#v, %v", decoded, err)
	}
}

func TestRejectsInvalidProsody(t *testing.T) {
	for name, mutate := range map[string]func(*qol.Prosody){
		"nan":           func(p *qol.Prosody) { p.PitchHz = float32(math.NaN()) },
		"negative rate": func(p *qol.Prosody) { p.Rate = -1 },
		"arousal":       func(p *qol.Prosody) { p.Arousal = 1.5 },
		"voiced":        func(p *qol.Prosody) { p.VoicedRatio = -0.1 },
	} {
		prosody := sampleProsody()
		mutate(prosody)
		if _, err := wire.MarshalText(qol.Text{Text: "hello", Language: "en", Prosody: prosody}); err == nil {
			t.Fatalf("%s: expected invalid prosody to fail", name)
		}
	}
}

func TestProcessorFramesRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	utterance := qol.Utterance{Span: qol.Span{Start: 1_000_000_000, End: 2_500_000_000}, Text: "good morning"}
	request := qol.SynthesisRequest{Text: "buenos días", Language: "es", Prosody: sampleProsody()}
	chunk := qol.SynthesisChunk{Audio: qol.Audio{Format: qol.PCMFormat{SampleRate: 22050, Channels: 1, Format: qol.SampleS16LE}, Data: []byte{1, 2, 3, 4}}, EndOfUtterance: true}
	if err := wire.WriteUtterance(&buffer, utterance); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteSynthesisRequest(&buffer, request); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteSynthesisChunk(&buffer, chunk); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(&buffer)
	gotUtterance, err := wire.ReadUtterance(reader)
	if err != nil || !reflect.DeepEqual(gotUtterance, utterance) {
		t.Fatalf("unexpected utterance: %#v, %v", gotUtterance, err)
	}
	gotRequest, err := wire.ReadSynthesisRequest(reader)
	if err != nil || !reflect.DeepEqual(gotRequest, request) {
		t.Fatalf("unexpected request: %#v, %v", gotRequest, err)
	}
	gotChunk, err := wire.ReadSynthesisChunk(reader)
	if err != nil || !reflect.DeepEqual(gotChunk, chunk) {
		t.Fatalf("unexpected chunk: %#v, %v", gotChunk, err)
	}
}

func TestRejectsInvalidProcessorFrames(t *testing.T) {
	var buffer bytes.Buffer
	if err := wire.WriteUtterance(&buffer, qol.Utterance{Span: qol.Span{Start: 2, End: 1}, Text: "x"}); err == nil {
		t.Fatal("expected reversed span to fail")
	}
	if err := wire.WriteSynthesisRequest(&buffer, qol.SynthesisRequest{Text: "hola"}); err == nil {
		t.Fatal("expected missing language to fail")
	}
	if err := wire.WriteSynthesisChunk(&buffer, qol.SynthesisChunk{}); err == nil {
		t.Fatal("expected missing format to fail")
	}
}
