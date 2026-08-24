package worker

import (
	"errors"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

func NewSTT(processor StreamProcessor, spoolDir string) *Stage {
	stage := newStage("stt-whisper", "qol-stt-whisper", "qol-stt-whisper", qol.ChannelAudioInput, []string{qol.TypeAudioStream}, qol.ChannelASRText, qol.TypeTextFinal, processor, spoolDir, decodeAudioStream, func(value chunk) ([]byte, error) {
		return wire.MarshalText(qol.Text{Text: string(value.data), Final: value.end, Language: "en"})
	})
	stage.live = true
	return stage
}

func decodeAudioStream(event qol.Event) (chunk, error) {
	if event.Type != qol.TypeAudioStream {
		return chunk{}, errors.New("stt requires an audio stream event")
	}
	_, _, end, data, err := wire.UnmarshalAudioStream(event.Payload)
	return chunk{data: data, end: end}, err
}
