package evaluation

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

func FromSynthesis(requests, chunks io.Reader) ([]Utterance, error) {
	var utterances []Utterance
	requestReader := bufio.NewReader(requests)
	for {
		request, err := wire.ReadSynthesisRequest(requestReader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read synthesis request %d: %w", len(utterances), err)
		}
		utterances = append(utterances, Utterance{Text: request.Text, Language: request.Language, Source: request.Prosody, Start: -1})
	}
	chunkReader := bufio.NewReader(chunks)
	for index := range utterances {
		for {
			chunk, err := wire.ReadSynthesisChunk(chunkReader)
			if err != nil {
				return nil, fmt.Errorf("read audio for utterance %d: %w", index, err)
			}
			if chunk.Audio.Format.Format != qol.SampleS16LE || chunk.Audio.Format.Channels != 1 {
				return nil, fmt.Errorf("utterance %d audio must be mono S16LE", index)
			}
			utterances[index].SampleRate = chunk.Audio.Format.SampleRate
			utterances[index].Samples = append(utterances[index].Samples, Samples(chunk.Audio.Data)...)
			if chunk.EndOfUtterance {
				break
			}
		}
	}
	return utterances, nil
}
