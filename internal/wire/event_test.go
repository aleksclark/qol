package wire_test

import (
	"reflect"
	"testing"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

func TestEventRoundTrip(t *testing.T) {
	payload, err := wire.MarshalText(qol.Text{Text: "hello", Final: true, Language: "en", Speaker: "speaker", Confidence: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	input := qol.Event{ID: "event", SessionID: "session", Channel: qol.ChannelASRText, Type: qol.TypeTextFinal, Seq: 2, Span: qol.Span{Start: 10, End: 20}, Parents: []string{"audio"}, ProducedAt: time.Unix(10, 0).UTC(), Encoding: qol.EncodingProtobuf, Payload: payload}
	message, err := wire.EventToProto(input)
	if err != nil {
		t.Fatal(err)
	}
	output, err := wire.EventFromProto(message)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(output, input) {
		t.Fatalf("unexpected round trip: %#v", output)
	}
}

func TestPayloadRoundTrips(t *testing.T) {
	audio := qol.Audio{Format: qol.PCMFormat{SampleRate: 16000, Channels: 1, Format: qol.SampleS16LE}, Data: []byte{1, 2}}
	audioPayload, err := wire.MarshalAudio(audio)
	if err != nil {
		t.Fatal(err)
	}
	audioOutput, err := wire.UnmarshalAudio(audioPayload)
	if err != nil || !reflect.DeepEqual(audioOutput, audio) {
		t.Fatalf("unexpected audio: %#v, %v", audioOutput, err)
	}
	translation := qol.Translation{Text: "hola", SourceLanguage: "en", TargetLanguage: "es", Final: true}
	translationPayload, err := wire.MarshalTranslation(translation)
	if err != nil {
		t.Fatal(err)
	}
	translationOutput, err := wire.UnmarshalTranslation(translationPayload)
	if err != nil || !reflect.DeepEqual(translationOutput, translation) {
		t.Fatalf("unexpected translation: %#v, %v", translationOutput, err)
	}
	stored := qol.StoredAudio{MediaType: "audio/mpeg", Path: "/output/session.mp3", SizeBytes: 10}
	storedPayload, err := wire.MarshalStoredAudio(stored)
	if err != nil {
		t.Fatal(err)
	}
	storedOutput, err := wire.UnmarshalStoredAudio(storedPayload)
	if err != nil || !reflect.DeepEqual(storedOutput, stored) {
		t.Fatalf("unexpected stored audio: %#v, %v", storedOutput, err)
	}
}

func TestRejectsMissingEventMetadata(t *testing.T) {
	_, err := wire.EventToProto(qol.Event{ID: "event", SessionID: "session"})
	if err == nil {
		t.Fatal("expected missing event metadata to fail")
	}
}
