package prosody

import (
	"math"
	"time"

	qol "github.com/aleksclark/qol"
)

const (
	DefaultReferenceRate = 4.5
	baselineWeight       = 0.15
	minBaselineSeconds   = 0.5
	longPause            = 800 * time.Millisecond
)

type Tracker struct {
	referenceRate float64
	pitch         float64
	energy        float64
	ready         bool
}

func NewTracker(referenceRate float64) *Tracker {
	if referenceRate <= 0 {
		referenceRate = DefaultReferenceRate
	}
	return &Tracker{referenceRate: referenceRate}
}

func (t *Tracker) Annotate(measurement Measurement, pauseBefore time.Duration) qol.Prosody {
	result := qol.Prosody{
		PitchHz:             float32(measurement.PitchHz),
		Energy:              float32(measurement.EnergyDB),
		Rate:                float32(measurement.Rate),
		Emphasis:            float32(measurement.Emphasis),
		PitchRangeSemitones: float32(measurement.PitchRangeSemitones),
		VoicedRatio:         float32(measurement.VoicedRatio),
		SpeechSeconds:       float32(measurement.SpeechSeconds),
		RelativeRate:        1,
	}
	if pauseBefore > 0 {
		result.PauseBeforeMillis = uint32(min(pauseBefore.Milliseconds(), math.MaxUint32))
	}
	if measurement.Rate > 0 {
		result.RelativeRate = float32(measurement.Rate / t.referenceRate)
	}
	pitch := semitones(measurement.PitchHz)
	usable := measurement.SpeechSeconds >= minBaselineSeconds
	if usable && !t.ready {
		t.pitch, t.energy, t.ready = pitch, measurement.EnergyDB, true
	}
	if t.ready {
		if measurement.PitchHz > 0 && t.pitch != 0 {
			result.RelativePitchSemitones = float32(pitch - t.pitch)
		}
		if measurement.SpeechSeconds > 0 {
			result.RelativeEnergyDB = float32(measurement.EnergyDB - t.energy)
		}
	}
	if usable {
		if measurement.PitchHz > 0 {
			if t.pitch == 0 {
				t.pitch = pitch
			}
			t.pitch += baselineWeight * (pitch - t.pitch)
		}
		t.energy += baselineWeight * (measurement.EnergyDB - t.energy)
	}
	result.Arousal = float32(arousal(result))
	result.Markers = markers(result, pauseBefore)
	return result
}

func arousal(value qol.Prosody) float64 {
	energy := clamp(float64(value.RelativeEnergyDB)/6, -1, 1)
	pitch := clamp(float64(value.RelativePitchSemitones)/4, -1, 1)
	pitchRange := 0.0
	if value.PitchRangeSemitones > 0 {
		pitchRange = clamp((float64(value.PitchRangeSemitones)-6)/6, -1, 1)
	}
	rate := clamp((float64(value.RelativeRate)-1)/0.4, -1, 1)
	return clamp(0.5+0.5*(0.4*energy+0.25*pitch+0.2*pitchRange+0.15*rate), 0, 1)
}

func markers(value qol.Prosody, pauseBefore time.Duration) []string {
	var result []string
	switch {
	case value.Rate > 0 && value.RelativeRate >= 1.15:
		result = append(result, "fast")
	case value.Rate > 0 && value.RelativeRate <= 0.87:
		result = append(result, "slow")
	}
	switch {
	case value.RelativeEnergyDB >= 4:
		result = append(result, "loud")
	case value.RelativeEnergyDB <= -4:
		result = append(result, "soft")
	}
	switch {
	case value.RelativePitchSemitones >= 2:
		result = append(result, "high")
	case value.RelativePitchSemitones <= -2:
		result = append(result, "low")
	}
	switch {
	case value.PitchRangeSemitones >= 9:
		result = append(result, "animated")
	case value.PitchRangeSemitones > 0 && value.PitchRangeSemitones <= 3:
		result = append(result, "monotone")
	}
	if value.Emphasis >= 0.6 {
		result = append(result, "emphatic")
	}
	if pauseBefore >= longPause {
		result = append(result, "long-pause")
	}
	return result
}

func semitones(hz float64) float64 {
	if hz <= 0 {
		return 0
	}
	return 12 * math.Log2(hz/55)
}
