package wire

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"

	qol "github.com/aleksclark/qol"
	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
)

const MaxReferenceBytes = 512 << 10

func ProsodyToProto(value *qol.Prosody) *qolv1.ProsodyPayload {
	if value == nil {
		return nil
	}
	return &qolv1.ProsodyPayload{
		PitchHz:                value.PitchHz,
		Energy:                 value.Energy,
		Rate:                   value.Rate,
		Emphasis:               value.Emphasis,
		PitchRangeSemitones:    value.PitchRangeSemitones,
		RelativePitchSemitones: value.RelativePitchSemitones,
		RelativeEnergyDb:       value.RelativeEnergyDB,
		RelativeRate:           value.RelativeRate,
		Arousal:                value.Arousal,
		PauseBeforeMs:          value.PauseBeforeMillis,
		VoicedRatio:            value.VoicedRatio,
		SpeechSeconds:          value.SpeechSeconds,
		Emotion:                value.Emotion,
		EmotionConfidence:      value.EmotionConfidence,
		Markers:                append([]string(nil), value.Markers...),

		RelativePitchRangeSemitones: value.RelativePitchRangeSemitones,
		EmotionScores:               emotionScoresToProto(value.EmotionScores),
		ReferenceAudio:              audioToProto(value.ReferenceAudio),
	}
}

func audioToProto(value *qol.Audio) *qolv1.AudioPayload {
	if value == nil {
		return nil
	}
	format, err := sampleFormatToProto(value.Format.Format)
	if err != nil {
		return nil
	}
	return &qolv1.AudioPayload{Format: &qolv1.PcmFormat{SampleRateHz: uint32(value.Format.SampleRate), ChannelCount: uint32(value.Format.Channels), SampleFormat: format}, Data: append([]byte(nil), value.Data...)}
}

func ProsodyFromProto(message *qolv1.ProsodyPayload) *qol.Prosody {
	if message == nil {
		return nil
	}
	return &qol.Prosody{
		PitchHz:                message.PitchHz,
		Energy:                 message.Energy,
		Rate:                   message.Rate,
		Emphasis:               message.Emphasis,
		PitchRangeSemitones:    message.PitchRangeSemitones,
		RelativePitchSemitones: message.RelativePitchSemitones,
		RelativeEnergyDB:       message.RelativeEnergyDb,
		RelativeRate:           message.RelativeRate,
		Arousal:                message.Arousal,
		PauseBeforeMillis:      message.PauseBeforeMs,
		VoicedRatio:            message.VoicedRatio,
		SpeechSeconds:          message.SpeechSeconds,
		Emotion:                message.Emotion,
		EmotionConfidence:      message.EmotionConfidence,
		Markers:                append([]string(nil), message.Markers...),

		RelativePitchRangeSemitones: message.RelativePitchRangeSemitones,
		EmotionScores:               emotionScoresFromProto(message.EmotionScores),
		ReferenceAudio:              referenceFromProto(message.ReferenceAudio),
	}
}

func referenceFromProto(message *qolv1.AudioPayload) *qol.Audio {
	if message == nil {
		return nil
	}
	audio, err := audioFromProto(message)
	if err != nil {
		return &qol.Audio{}
	}
	return &audio
}

func emotionScoresToProto(scores []qol.EmotionScore) []*qolv1.EmotionScore {
	var result []*qolv1.EmotionScore
	for _, score := range scores {
		result = append(result, &qolv1.EmotionScore{Label: score.Label, Score: score.Score})
	}
	return result
}

func emotionScoresFromProto(scores []*qolv1.EmotionScore) []qol.EmotionScore {
	var result []qol.EmotionScore
	for _, score := range scores {
		result = append(result, qol.EmotionScore{Label: score.GetLabel(), Score: score.GetScore()})
	}
	return result
}

func ValidateProsody(value *qol.Prosody) error {
	if value == nil {
		return nil
	}
	for _, field := range []float32{value.PitchHz, value.Energy, value.Rate, value.Emphasis, value.PitchRangeSemitones, value.RelativePitchSemitones, value.RelativePitchRangeSemitones, value.RelativeEnergyDB, value.RelativeRate, value.Arousal, value.VoicedRatio, value.SpeechSeconds, value.EmotionConfidence} {
		if math.IsNaN(float64(field)) || math.IsInf(float64(field), 0) {
			return errors.New("prosody values must be finite")
		}
	}
	if value.PitchHz < 0 || value.Rate < 0 || value.RelativeRate < 0 || value.SpeechSeconds < 0 {
		return errors.New("prosody pitch, rate, and duration must not be negative")
	}
	if value.Emphasis < 0 || value.Emphasis > 1 || value.Arousal < 0 || value.Arousal > 1 || value.VoicedRatio < 0 || value.VoicedRatio > 1 {
		return errors.New("prosody emphasis, arousal, and voiced ratio must be within 0..1")
	}
	if value.EmotionConfidence < 0 || value.EmotionConfidence > 1 {
		return errors.New("prosody emotion confidence must be within 0..1")
	}
	if reference := value.ReferenceAudio; reference != nil {
		if reference.Format.Format != qol.SampleS16LE || reference.Format.Channels != 1 || reference.Format.SampleRate <= 0 || len(reference.Data) == 0 || len(reference.Data)%2 != 0 {
			return errors.New("prosody reference audio must be non-empty mono S16LE PCM")
		}
		if len(reference.Data) > MaxReferenceBytes {
			return fmt.Errorf("prosody reference audio exceeds %d bytes", MaxReferenceBytes)
		}
	}
	for _, score := range value.EmotionScores {
		if score.Label == "" || math.IsNaN(float64(score.Score)) || score.Score < 0 || score.Score > 1 {
			return errors.New("emotion scores need a label and a 0..1 score")
		}
	}
	return nil
}

func WriteUtterance(writer io.Writer, value qol.Utterance) error {
	if value.Text == "" || value.Span.End < value.Span.Start {
		return errors.New("utterance text and ordered span are required")
	}
	return WriteFrame(writer, &qolv1.RecognizedUtterance{Span: &qolv1.MediaSpan{StartNs: int64(value.Span.Start), EndNs: int64(value.Span.End)}, Text: value.Text})
}

func ReadUtterance(reader *bufio.Reader) (qol.Utterance, error) {
	message := new(qolv1.RecognizedUtterance)
	if err := ReadFrame(reader, message); err != nil {
		return qol.Utterance{}, err
	}
	value := qol.Utterance{Span: qol.Span{Start: qol.MediaTime(message.GetSpan().GetStartNs()), End: qol.MediaTime(message.GetSpan().GetEndNs())}, Text: message.Text}
	if value.Text == "" || value.Span.End < value.Span.Start || value.Span.Start < 0 {
		return qol.Utterance{}, errors.New("utterance text and ordered span are required")
	}
	return value, nil
}

func WriteSynthesisRequest(writer io.Writer, value qol.SynthesisRequest) error {
	if value.Text == "" || value.Language == "" {
		return errors.New("synthesis text and language are required")
	}
	if err := ValidateProsody(value.Prosody); err != nil {
		return err
	}
	if math.IsNaN(float64(value.Pace)) || value.Pace < 0 {
		return errors.New("synthesis pace must be a non-negative number")
	}
	return WriteFrame(writer, &qolv1.SynthesisRequest{Text: value.Text, Language: value.Language, Prosody: ProsodyToProto(value.Prosody), Pace: value.Pace})
}

func ReadSynthesisRequest(reader *bufio.Reader) (qol.SynthesisRequest, error) {
	message := new(qolv1.SynthesisRequest)
	if err := ReadFrame(reader, message); err != nil {
		return qol.SynthesisRequest{}, err
	}
	value := qol.SynthesisRequest{Text: message.Text, Language: message.Language, Prosody: ProsodyFromProto(message.Prosody), Pace: message.Pace}
	if value.Text == "" || value.Language == "" {
		return qol.SynthesisRequest{}, errors.New("synthesis text and language are required")
	}
	return value, ValidateProsody(value.Prosody)
}

func WriteSynthesisChunk(writer io.Writer, value qol.SynthesisChunk) error {
	format, err := sampleFormatToProto(value.Audio.Format.Format)
	if err != nil {
		return err
	}
	return WriteFrame(writer, &qolv1.SynthesisChunk{Format: &qolv1.PcmFormat{SampleRateHz: uint32(value.Audio.Format.SampleRate), ChannelCount: uint32(value.Audio.Format.Channels), SampleFormat: format}, Data: value.Audio.Data, EndOfUtterance: value.EndOfUtterance})
}

func ReadSynthesisChunk(reader *bufio.Reader) (qol.SynthesisChunk, error) {
	message := new(qolv1.SynthesisChunk)
	if err := ReadFrame(reader, message); err != nil {
		return qol.SynthesisChunk{}, err
	}
	if message.Format == nil || message.Format.SampleRateHz == 0 || message.Format.ChannelCount == 0 {
		return qol.SynthesisChunk{}, errors.New("synthesis chunk format is required")
	}
	format, err := sampleFormatFromProto(message.Format.SampleFormat)
	if err != nil {
		return qol.SynthesisChunk{}, err
	}
	return qol.SynthesisChunk{Audio: qol.Audio{Format: qol.PCMFormat{SampleRate: int(message.Format.SampleRateHz), Channels: int(message.Format.ChannelCount), Format: format}, Data: append([]byte(nil), message.Data...)}, EndOfUtterance: message.EndOfUtterance}, nil
}

func WriteEmotionRequest(writer io.Writer, value qol.Audio) error {
	format, err := sampleFormatToProto(value.Format.Format)
	if err != nil {
		return err
	}
	if value.Format.SampleRate <= 0 || value.Format.Channels <= 0 || len(value.Data) == 0 {
		return errors.New("emotion request audio format and data are required")
	}
	return WriteFrame(writer, &qolv1.AudioPayload{Format: &qolv1.PcmFormat{SampleRateHz: uint32(value.Format.SampleRate), ChannelCount: uint32(value.Format.Channels), SampleFormat: format}, Data: value.Data})
}

func ReadEmotionRequest(reader *bufio.Reader) (qol.Audio, error) {
	message := new(qolv1.AudioPayload)
	if err := ReadFrame(reader, message); err != nil {
		return qol.Audio{}, err
	}
	return audioFromProto(message)
}

func WriteEmotionResult(writer io.Writer, value qol.EmotionResult) error {
	if err := validateEmotion(value); err != nil {
		return err
	}
	return WriteFrame(writer, &qolv1.EmotionResult{Label: value.Label, Confidence: value.Confidence, Scores: emotionScoresToProto(value.Scores)})
}

func ReadEmotionResult(reader *bufio.Reader) (qol.EmotionResult, error) {
	message := new(qolv1.EmotionResult)
	if err := ReadFrame(reader, message); err != nil {
		return qol.EmotionResult{}, err
	}
	value := qol.EmotionResult{Label: message.Label, Confidence: message.Confidence, Scores: emotionScoresFromProto(message.Scores)}
	return value, validateEmotion(value)
}

func validateEmotion(value qol.EmotionResult) error {
	if value.Label == "" || math.IsNaN(float64(value.Confidence)) || value.Confidence < 0 || value.Confidence > 1 {
		return errors.New("emotion result needs a label and a 0..1 confidence")
	}
	return ValidateProsody(&qol.Prosody{EmotionScores: value.Scores})
}
