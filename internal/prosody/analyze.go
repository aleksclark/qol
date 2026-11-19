package prosody

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	frameDuration   = 25 * time.Millisecond
	hopDuration     = 10 * time.Millisecond
	minPitchHz      = 60.0
	maxPitchHz      = 420.0
	yinThreshold    = 0.15
	silenceFloorDB  = -55.0
	minRateSeconds  = 0.25
	referenceSample = 32768.0
)

type Measurement struct {
	PitchHz             float64
	PitchRangeSemitones float64
	EnergyDB            float64
	Emphasis            float64
	Rate                float64
	Syllables           int
	SpeechSeconds       float64
	VoicedRatio         float64
	LeadingSilence      time.Duration
	SpeechEnd           time.Duration
	Duration            time.Duration
}

func Analyze(samples []int16, sampleRate int, syllables int) Measurement {
	result := Measurement{Syllables: syllables}
	if sampleRate <= 0 || len(samples) == 0 {
		return result
	}
	result.Duration = time.Duration(len(samples)) * time.Second / time.Duration(sampleRate)
	frame := int(int64(sampleRate) * int64(frameDuration) / int64(time.Second))
	hop := int(int64(sampleRate) * int64(hopDuration) / int64(time.Second))
	if frame <= 0 || hop <= 0 || len(samples) < frame {
		return result
	}
	signal := make([]float64, len(samples))
	for index, sample := range samples {
		signal[index] = float64(sample) / referenceSample
	}
	var levels []float64
	for start := 0; start+frame <= len(signal); start += hop {
		levels = append(levels, levelDB(signal[start:start+frame]))
	}
	threshold := activityThreshold(levels)
	tauMin := int(float64(sampleRate) / maxPitchHz)
	tauMax := int(float64(sampleRate) / minPitchHz)
	var active, pitches, activeLevels []float64
	firstActive, lastActive := -1, -1
	for index, level := range levels {
		if level < threshold {
			continue
		}
		if firstActive < 0 {
			firstActive = index
		}
		lastActive = index
		activeLevels = append(activeLevels, level)
		active = append(active, math.Pow(10, level/10))
		start := index * hop
		if start+frame+tauMax > len(signal) {
			continue
		}
		if pitch, ok := yin(signal[start:start+frame+tauMax], frame, tauMin, tauMax, sampleRate); ok {
			pitches = append(pitches, pitch)
		}
	}
	if len(active) == 0 {
		result.LeadingSilence = result.Duration
		return result
	}
	result.LeadingSilence = time.Duration(firstActive) * hopDuration
	result.SpeechEnd = min(time.Duration(lastActive)*hopDuration+frameDuration, result.Duration)
	result.SpeechSeconds = float64(len(active)) * hopDuration.Seconds()
	result.EnergyDB = 10 * math.Log10(mean(active))
	result.VoicedRatio = float64(len(pitches)) / float64(len(active))
	sort.Float64s(activeLevels)
	contrast := percentile(activeLevels, 0.95) - percentile(activeLevels, 0.5)
	result.Emphasis = clamp((contrast-3)/9, 0, 1)
	if len(pitches) > 0 {
		sort.Float64s(pitches)
		result.PitchHz = percentile(pitches, 0.5)
		low, high := percentile(pitches, 0.1), percentile(pitches, 0.9)
		if low > 0 {
			result.PitchRangeSemitones = 12 * math.Log2(high/low)
		}
	}
	if result.SpeechSeconds >= minRateSeconds {
		result.Rate = float64(result.Syllables) / result.SpeechSeconds
	}
	return result
}

func CountSyllables(text, language string) int {
	total := 0
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
		word = strings.Trim(word, "'")
		if word == "" {
			continue
		}
		groups := 0
		previousVowel := false
		for _, letter := range word {
			vowel := strings.ContainsRune("aeiouyáéíóúü", letter)
			if vowel && !previousVowel {
				groups++
			}
			previousVowel = vowel
		}
		if language == "en" && groups > 1 && strings.HasSuffix(word, "e") && !strings.HasSuffix(word, "le") {
			groups--
		}
		if groups == 0 {
			groups = 1
		}
		total += groups
	}
	return total
}

func yin(window []float64, size, tauMin, tauMax, sampleRate int) (float64, bool) {
	difference := make([]float64, tauMax+1)
	for tau := 1; tau <= tauMax; tau++ {
		var sum float64
		for index := 0; index < size; index++ {
			delta := window[index] - window[index+tau]
			sum += delta * delta
		}
		difference[tau] = sum
	}
	normalized := make([]float64, tauMax+1)
	normalized[0] = 1
	var running float64
	for tau := 1; tau <= tauMax; tau++ {
		running += difference[tau]
		if running == 0 {
			normalized[tau] = 1
			continue
		}
		normalized[tau] = difference[tau] * float64(tau) / running
	}
	for tau := tauMin; tau <= tauMax; tau++ {
		if normalized[tau] >= yinThreshold {
			continue
		}
		for tau+1 <= tauMax && normalized[tau+1] < normalized[tau] {
			tau++
		}
		estimate := float64(tau)
		if tau > 1 && tau < tauMax {
			left, center, right := normalized[tau-1], normalized[tau], normalized[tau+1]
			denominator := left + right - 2*center
			if denominator != 0 {
				estimate += (left - right) / (2 * denominator)
			}
		}
		return float64(sampleRate) / estimate, true
	}
	return 0, false
}

func levelDB(frame []float64) float64 {
	var power float64
	for _, sample := range frame {
		power += sample * sample
	}
	power /= float64(len(frame))
	if power <= 1e-12 {
		return -120
	}
	return 10 * math.Log10(power)
}

func activityThreshold(levels []float64) float64 {
	sorted := append([]float64(nil), levels...)
	sort.Float64s(sorted)
	threshold := math.Min(percentile(sorted, 0.1)+10, percentile(sorted, 0.95)-25)
	return math.Max(threshold, silenceFloorDB)
}

func percentile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	position := fraction * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func mean(values []float64) float64 {
	var sum float64
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func clamp(value, low, high float64) float64 {
	return math.Max(low, math.Min(high, value))
}
