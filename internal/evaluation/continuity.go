package evaluation

import (
	"math"
	"time"
)

type Continuity struct {
	Seconds       float64
	AbruptOnsets  int
	OnsetsPerMin  float64
	AbruptOnsetAt []time.Duration
}

func Discontinuities(samples []int16, rate int) Continuity {
	result := Continuity{Seconds: float64(len(samples)) / float64(max(rate, 1))}
	if rate <= 0 || len(samples) < rate/10 {
		return result
	}
	hop := rate / 100
	previous := -120.0
	for start := 0; start+hop <= len(samples); start += hop {
		var power float64
		for _, sample := range samples[start : start+hop] {
			value := float64(sample) / 32768
			power += value * value
		}
		level := 10 * math.Log10(power/float64(hop)+1e-12)
		if previous < -60 && level > -25 {
			result.AbruptOnsets++
			result.AbruptOnsetAt = append(result.AbruptOnsetAt, time.Duration(start)*time.Second/time.Duration(rate))
		}
		previous = level
	}
	if minutes := result.Seconds / 60; minutes > 0 {
		result.OnsetsPerMin = float64(result.AbruptOnsets) / minutes
	}
	return result
}

type Joins struct {
	Count      int
	Tight      int
	MinimumGap time.Duration
}

func PhraseJoins(utterances []Utterance, tight time.Duration) Joins {
	result := Joins{MinimumGap: -1}
	var previous *Utterance
	for index := range utterances {
		current := &utterances[index]
		if len(current.Samples) == 0 {
			continue
		}
		if previous != nil {
			gap := silence(previous.Samples, previous.SampleRate, true) + silence(current.Samples, current.SampleRate, false)
			result.Count++
			if gap < tight {
				result.Tight++
			}
			if result.MinimumGap < 0 || gap < result.MinimumGap {
				result.MinimumGap = gap
			}
		}
		previous = current
	}
	return result
}

func silence(samples []int16, rate int, trailing bool) time.Duration {
	count := 0
	for index := range samples {
		sample := samples[index]
		if trailing {
			sample = samples[len(samples)-1-index]
		}
		if math.Abs(float64(sample))/32768 > 0.003 {
			break
		}
		count++
	}
	return time.Duration(count) * time.Second / time.Duration(max(rate, 1))
}
