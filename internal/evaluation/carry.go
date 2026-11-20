package evaluation

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/prosody"
	"github.com/aleksclark/qol/internal/wire"
)

type Utterance struct {
	Span       qol.Span
	Text       string
	Language   string
	Source     *qol.Prosody
	Start      time.Duration
	Samples    []int16
	SampleRate int
}

type Row struct {
	Utterance Utterance
	Output    prosody.Measurement
	Emotion   qol.EmotionResult
}

type Report struct {
	Utterances      int
	Annotated       int
	Rate            float64
	Energy          float64
	Pitch           float64
	PitchRange      float64
	LagMedian       time.Duration
	LagP90          time.Duration
	LagMax          time.Duration
	EmotionMatches  int
	EmotionCompared int
	SourceEmotions  map[string]int
	Rows            []Row
}

type Classifier func([]int16, int) (qol.EmotionResult, error)

func CarryThrough(utterances []Utterance, classify Classifier) (Report, error) {
	report := Report{SourceEmotions: map[string]int{}}
	var sourceRate, outputRate, sourceEnergy, outputEnergy, sourcePitch, outputPitch, sourceRange, outputRange []float64
	var lags []time.Duration
	for _, item := range utterances {
		report.Utterances++
		language := item.Language
		if language == "" {
			language = "es"
		}
		measured := prosody.Analyze(item.Samples, item.SampleRate, prosody.CountSyllables(item.Text, language))
		row := Row{Utterance: item, Output: measured}
		if item.Start >= 0 && measured.SpeechSeconds > 0 {
			lags = append(lags, item.Start+measured.LeadingSilence-time.Duration(item.Span.Start))
		}
		if item.Source == nil {
			report.Rows = append(report.Rows, row)
			continue
		}
		report.Annotated++
		if classify != nil && item.Source.Emotion != "" && measured.SpeechSeconds > 0 {
			emotion, err := classify(item.Samples, item.SampleRate)
			if err != nil {
				return Report{}, fmt.Errorf("classify output emotion: %w", err)
			}
			row.Emotion = emotion
			report.EmotionCompared++
			report.SourceEmotions[item.Source.Emotion]++
			if emotion.Label == item.Source.Emotion {
				report.EmotionMatches++
			}
		}
		report.Rows = append(report.Rows, row)
		if item.Source.Rate == 0 || measured.Rate == 0 || measured.PitchHz == 0 {
			continue
		}
		sourceRate = append(sourceRate, float64(item.Source.RelativeRate))
		outputRate = append(outputRate, measured.Rate)
		sourceEnergy = append(sourceEnergy, float64(item.Source.RelativeEnergyDB))
		outputEnergy = append(outputEnergy, measured.EnergyDB)
		sourcePitch = append(sourcePitch, float64(item.Source.RelativePitchSemitones))
		outputPitch = append(outputPitch, 12*math.Log2(measured.PitchHz))
		sourceRange = append(sourceRange, float64(item.Source.RelativePitchRangeSemitones))
		outputRange = append(outputRange, measured.PitchRangeSemitones)
	}
	report.Rate = Pearson(sourceRate, outputRate)
	report.Energy = Pearson(sourceEnergy, outputEnergy)
	report.Pitch = Pearson(sourcePitch, outputPitch)
	report.PitchRange = Pearson(sourceRange, outputRange)
	if len(lags) > 0 {
		slices.Sort(lags)
		report.LagMedian = lags[len(lags)/2]
		report.LagP90 = lags[len(lags)*9/10]
		report.LagMax = lags[len(lags)-1]
	}
	return report, nil
}

func FromGraphEvents(translations, audio []qol.Event) ([]Utterance, error) {
	bySpan := map[qol.Span]int{}
	var utterances []Utterance
	for _, event := range translations {
		translation, err := wire.UnmarshalTranslation(event.Payload)
		if err != nil {
			return nil, err
		}
		if translation.Final {
			continue
		}
		bySpan[event.Span] = len(utterances)
		utterances = append(utterances, Utterance{Span: event.Span, Text: translation.Text, Language: translation.TargetLanguage, Source: translation.Prosody, Start: -1})
	}
	ordered := append([]qol.Event(nil), audio...)
	slices.SortFunc(ordered, func(left, right qol.Event) int { return int(left.Seq) - int(right.Seq) })
	var position int
	for _, event := range ordered {
		decoded, err := wire.UnmarshalAudio(event.Payload)
		if err != nil {
			return nil, err
		}
		if event.Type == qol.TypeAudioPCM {
			index, ok := bySpan[event.Span]
			if !ok {
				return nil, fmt.Errorf("audio for unknown span %+v", event.Span)
			}
			target := &utterances[index]
			target.SampleRate = decoded.Format.SampleRate
			if target.Start < 0 {
				target.Start = time.Duration(position) * time.Second / time.Duration(decoded.Format.SampleRate)
			}
			target.Samples = append(target.Samples, Samples(decoded.Data)...)
		}
		position += len(decoded.Data) / 2
	}
	return utterances, nil
}

func Samples(data []byte) []int16 {
	samples := make([]int16, len(data)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(data[index*2:]))
	}
	return samples
}

func Pearson(left, right []float64) float64 {
	if len(left) < 3 || len(left) != len(right) {
		return math.NaN()
	}
	var meanLeft, meanRight float64
	for index := range left {
		meanLeft += left[index]
		meanRight += right[index]
	}
	meanLeft /= float64(len(left))
	meanRight /= float64(len(right))
	var covariance, varianceLeft, varianceRight float64
	for index := range left {
		covariance += (left[index] - meanLeft) * (right[index] - meanRight)
		varianceLeft += (left[index] - meanLeft) * (left[index] - meanLeft)
		varianceRight += (right[index] - meanRight) * (right[index] - meanRight)
	}
	if varianceLeft == 0 || varianceRight == 0 {
		return math.NaN()
	}
	return covariance / math.Sqrt(varianceLeft*varianceRight)
}

func (r Report) Summary() string {
	summary := fmt.Sprintf("utterances=%d annotated=%d | correlation rate=%.2f level=%.2f pitch=%.2f range=%.2f", r.Utterances, r.Annotated, r.Rate, r.Energy, r.Pitch, r.PitchRange)
	if r.LagMax > 0 {
		summary += fmt.Sprintf(" | onset lag median=%s p90=%s max=%s", r.LagMedian.Round(time.Millisecond), r.LagP90.Round(time.Millisecond), r.LagMax.Round(time.Millisecond))
	}
	if r.EmotionCompared > 0 {
		summary += fmt.Sprintf(" | emotion agreement %d/%d %v", r.EmotionMatches, r.EmotionCompared, r.SourceEmotions)
	}
	return summary
}

func (row Row) String() string {
	source := row.Utterance.Source
	if source == nil {
		source = &qol.Prosody{}
	}
	return fmt.Sprintf("%6.1fs src rate=%.2f relE=%+.1fdB relP=%+.1fst relR=%+.1fst emo=%s/%.2f markers=%v | out start=%.1fs rate=%.2f E=%.1fdB P=%.0fHz R=%.1fst emo=%s/%.2f | %s",
		time.Duration(row.Utterance.Span.Start).Seconds(), source.RelativeRate, source.RelativeEnergyDB, source.RelativePitchSemitones, source.RelativePitchRangeSemitones, source.Emotion, source.EmotionConfidence, source.Markers,
		(row.Utterance.Start + row.Output.LeadingSilence).Seconds(), row.Output.Rate, row.Output.EnergyDB, row.Output.PitchHz, row.Output.PitchRangeSemitones, row.Emotion.Label, row.Emotion.Confidence, row.Utterance.Text)
}
