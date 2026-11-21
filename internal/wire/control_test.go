package wire_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

func TestControlRoundTripsAndValidation(t *testing.T) {
	for _, control := range []qol.Control{
		{Kind: qol.ControlCondense, TargetSequences: []uint64{3, 4}, TargetRatio: 0.7},
		{Kind: qol.ControlSummarize},
		{Kind: qol.ControlGlossary, Glossary: []qol.GlossaryEntry{{Source: "Acts", Target: "Hechos"}}},
	} {
		payload, err := wire.MarshalControl(control)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := wire.UnmarshalControl(payload)
		if err != nil || !reflect.DeepEqual(decoded, control) {
			t.Fatalf("unexpected control: %#v %v", decoded, err)
		}
	}
	for name, control := range map[string]qol.Control{
		"no kind":        {},
		"no sequences":   {Kind: qol.ControlCondense, TargetRatio: 0.5},
		"ratio one":      {Kind: qol.ControlCondense, TargetSequences: []uint64{1}, TargetRatio: 1},
		"nan ratio":      {Kind: qol.ControlCondense, TargetSequences: []uint64{1}, TargetRatio: float32(math.NaN())},
		"empty glossary": {Kind: qol.ControlGlossary},
		"blank entry":    {Kind: qol.ControlGlossary, Glossary: []qol.GlossaryEntry{{Source: "x"}}},
	} {
		if _, err := wire.MarshalControl(control); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

func TestTelemetryAndReplacementRoundTrip(t *testing.T) {
	telemetry := qol.Telemetry{Stage: "tts", ConsumedThrough: 5, OutputCursor: 9, BacklogSeconds: 2.5, QueuedSequences: []uint64{7, 8}, ProjectedLagSeconds: 6.5, ProcessingMillis: 120, ContextTurns: 4}
	payload, err := wire.MarshalTelemetry(telemetry)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := wire.UnmarshalTelemetry(payload)
	if err != nil || !reflect.DeepEqual(decoded, telemetry) {
		t.Fatalf("unexpected telemetry: %#v %v", decoded, err)
	}
	if _, err := wire.MarshalTelemetry(qol.Telemetry{Stage: "tts", BacklogSeconds: math.Inf(1)}); err == nil {
		t.Fatal("expected infinite backlog to fail")
	}
	if _, err := wire.MarshalTelemetry(qol.Telemetry{}); err == nil {
		t.Fatal("expected missing stage to fail")
	}
	translation := qol.Translation{Text: "breve", SourceLanguage: "en", TargetLanguage: "es", Replaces: []uint64{3, 4}}
	payload, err = wire.MarshalTranslation(translation)
	if err != nil {
		t.Fatal(err)
	}
	decodedTranslation, err := wire.UnmarshalTranslation(payload)
	if err != nil || !reflect.DeepEqual(decodedTranslation, translation) {
		t.Fatalf("unexpected translation: %#v %v", decodedTranslation, err)
	}
}

func TestTranslatorFramesRoundTrip(t *testing.T) {
	requests := []qol.TranslatorRequest{
		{SessionID: "s", Kind: qol.TranslatorTurn, TurnID: 2, Text: "so here is the"},
		{SessionID: "s", Kind: qol.TranslatorTurn, TurnID: 3, Final: true},
		{SessionID: "s", Kind: qol.TranslatorCondense, TurnIDs: []uint64{2, 3}, TargetRatio: 0.6},
		{SessionID: "s", Kind: qol.TranslatorSummarize},
		{SessionID: "s", Kind: qol.TranslatorGlossary, Glossary: []qol.GlossaryEntry{{Source: "Peter", Target: "Pedro"}}},
		{SessionID: "s", Kind: qol.TranslatorEnd},
	}
	var buffer bytes.Buffer
	for _, request := range requests {
		if err := wire.WriteTranslatorRequest(&buffer, request); err != nil {
			t.Fatal(err)
		}
	}
	if err := wire.WriteTranslatorResponse(&buffer, qol.TranslatorResponse{Text: "así que", ContextTurns: 3}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(&buffer)
	for _, want := range requests {
		got, err := wire.ReadTranslatorRequest(reader)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected request: %#v %v", got, err)
		}
	}
	response, err := wire.ReadTranslatorResponse(reader)
	if err != nil || response.Text != "así que" || response.ContextTurns != 3 {
		t.Fatalf("unexpected response: %#v %v", response, err)
	}
	if _, err := wire.ReadTranslatorRequest(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
	for name, request := range map[string]qol.TranslatorRequest{
		"no session":   {Kind: qol.TranslatorSummarize},
		"blank turn":   {SessionID: "s", Kind: qol.TranslatorTurn, Text: " "},
		"bad condense": {SessionID: "s", Kind: qol.TranslatorCondense, TurnIDs: []uint64{1}, TargetRatio: 1.2},
		"unknown":      {SessionID: "s"},
	} {
		if err := wire.WriteTranslatorRequest(&buffer, request); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}
