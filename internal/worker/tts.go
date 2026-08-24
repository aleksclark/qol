package worker

import (
	"errors"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

func NewTTS(processor StreamProcessor, spoolDir string) *Stage {
	stage := newStage("tts", "qol-tts", "qol-tts", qol.ChannelTranslationText, []string{qol.TypeTranslationFinal}, qol.ChannelTTSAudio, qol.TypeAudioPCM, processor, spoolDir, decodeTranslation, func(value chunk) ([]byte, error) {
		return wire.MarshalAudio(qol.Audio{Format: qol.PCMFormat{SampleRate: 22050, Channels: 1, Format: qol.SampleS16LE}, Data: value.data})
	})
	stage.eager = true
	return stage
}

func decodeTranslation(event qol.Event) (chunk, error) {
	if event.Type != qol.TypeTranslationFinal {
		return chunk{}, errors.New("tts requires final translation")
	}
	translation, err := wire.UnmarshalTranslation(event.Payload)
	if err != nil {
		return chunk{}, err
	}
	if translation.TargetLanguage != "es" {
		return chunk{}, errors.New("tts requires Spanish translation")
	}
	return chunk{data: []byte(translation.Text), end: translation.Final}, nil
}
