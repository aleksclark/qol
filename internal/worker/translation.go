package worker

import (
	"errors"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

func NewTranslate(processor StreamProcessor, spoolDir string) *Stage {
	stage := newStage("translate", "qol-translate", "qol-translate", qol.ChannelASRText, []string{qol.TypeTextFinal}, qol.ChannelTranslationText, qol.TypeTranslationFinal, processor, spoolDir, decodeText, func(value chunk) ([]byte, error) {
		return wire.MarshalTranslation(qol.Translation{Text: string(value.data), SourceLanguage: "en", TargetLanguage: "es", Final: value.end})
	})
	stage.eager = true
	return stage
}

func decodeText(event qol.Event) (chunk, error) {
	if event.Type != qol.TypeTextFinal {
		return chunk{}, errors.New("translate requires final text")
	}
	text, err := wire.UnmarshalText(event.Payload)
	if err != nil {
		return chunk{}, err
	}
	if text.Language != "en" {
		return chunk{}, errors.New("translate requires English text")
	}
	return chunk{data: []byte(text.Text), end: text.Final}, nil
}
