package main

import (
	"testing"

	qol "github.com/aleksclark/qol"
)

func TestBuildStageWiresTheProsodyGraph(t *testing.T) {
	for stageType, input := range map[string]string{
		"capture":     qol.ChannelCaptureInput,
		"prosody":     qol.ChannelUtterances,
		"stt-whisper": qol.ChannelAudioInput,
		"translate":   qol.ChannelASRText,
		"tts":         qol.ChannelTranslationText,
		"conductor":   qol.ChannelTelemetry,
	} {
		stage, err := buildStage(stageOptions{stageType: stageType, processor: "/bin/true", ffmpeg: "ffmpeg"})
		if err != nil {
			t.Fatalf("%s: %v", stageType, err)
		}
		if got := stage.Spec().Inputs[0].Channel; got != input {
			t.Fatalf("%s reads %s, want %s", stageType, got, input)
		}
	}
	record, err := buildStage(stageOptions{stageType: "record", name: "record-es", input: qol.ChannelTTSAudio, output: qol.ChannelRecordESCompleted})
	if err != nil || record.Spec().Name != "record-es" {
		t.Fatalf("record: %#v, %v", record, err)
	}
}

func TestBuildStageSelectsTheStreamingTranslator(t *testing.T) {
	stage, err := buildStage(stageOptions{stageType: "translate", processor: "/bin/true", translator: "llm"})
	if err != nil {
		t.Fatal(err)
	}
	if inputs := stage.Spec().Inputs; len(inputs) != 2 || inputs[1].Channel != qol.ChannelTranslateControl {
		t.Fatalf("the llm translator must accept control: %#v", inputs)
	}
}

func TestBuildStageRejectsMissingConfiguration(t *testing.T) {
	for _, options := range []stageOptions{
		{stageType: "tts"},
		{stageType: "record", name: "record-en"},
		{stageType: "unknown"},
		{stageType: "translate", processor: "/bin/true", translator: "neural"},
	} {
		if _, err := buildStage(options); err == nil {
			t.Fatalf("expected %q to fail", options.stageType)
		}
	}
}
