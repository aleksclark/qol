package prosody_test

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/aleksclark/qol/internal/prosody"
)

const sampleRate = 16000

func silence(duration time.Duration) []int16 {
	return make([]int16, int(duration.Seconds()*sampleRate))
}

func voice(duration time.Duration, startHz, endHz, amplitude float64) []int16 {
	count := int(duration.Seconds() * sampleRate)
	samples := make([]int16, count)
	var phase float64
	for index := range samples {
		progress := float64(index) / float64(count)
		frequency := startHz * math.Pow(endHz/startHz, progress)
		phase += 2 * math.Pi * frequency / sampleRate
		value := math.Sin(phase) + 0.5*math.Sin(2*phase) + 0.25*math.Sin(3*phase)
		samples[index] = int16(amplitude * 32767 * value / 1.75)
	}
	return samples
}

func concat(parts ...[]int16) []int16 {
	var result []int16
	for _, part := range parts {
		result = append(result, part...)
	}
	return result
}

func near(t *testing.T, name string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("%s = %.3f, want %.3f ± %.3f", name, got, want, tolerance)
	}
}

func TestAnalyzeSteadyVoice(t *testing.T) {
	samples := concat(silence(200*time.Millisecond), voice(time.Second, 150, 150, 0.3), silence(300*time.Millisecond))
	got := prosody.Analyze(samples, sampleRate, prosody.CountSyllables("hello there friend", "en"))
	near(t, "pitch", got.PitchHz, 150, 3)
	near(t, "range", got.PitchRangeSemitones, 0, 0.5)
	near(t, "speech seconds", got.SpeechSeconds, 1, 0.06)
	near(t, "leading silence", got.LeadingSilence.Seconds(), 0.2, 0.03)
	near(t, "speech end", got.SpeechEnd.Seconds(), 1.2, 0.03)
	near(t, "rate", got.Rate, 4, 0.3)
	if got.Syllables != 4 || got.VoicedRatio < 0.9 {
		t.Fatalf("unexpected measurement: %+v", got)
	}
	near(t, "duration", got.Duration.Seconds(), 1.5, 0.001)
}

func TestAnalyzeGlideAndLevel(t *testing.T) {
	quiet := prosody.Analyze(voice(time.Second, 120, 240, 0.1), sampleRate, 2)
	loud := prosody.Analyze(voice(time.Second, 120, 240, 0.2), sampleRate, 2)
	if quiet.PitchRangeSemitones < 8 || quiet.PitchRangeSemitones > 11 {
		t.Fatalf("glide range = %.2f semitones", quiet.PitchRangeSemitones)
	}
	near(t, "level difference", loud.EnergyDB-quiet.EnergyDB, 6.02, 0.2)
	near(t, "median pitch", quiet.PitchHz, 170, 8)
}

func TestAnalyzeSilenceAndEmptyInput(t *testing.T) {
	got := prosody.Analyze(silence(time.Second), sampleRate, prosody.CountSyllables("nothing", "en"))
	if got.SpeechSeconds != 0 || got.Rate != 0 || got.PitchHz != 0 || got.LeadingSilence != time.Second {
		t.Fatalf("silence must not produce speech: %+v", got)
	}
	if empty := prosody.Analyze(nil, sampleRate, 0); empty.Duration != 0 || empty.SpeechSeconds != 0 {
		t.Fatalf("empty input: %+v", empty)
	}
}

func TestCountSyllables(t *testing.T) {
	for text, want := range map[string]int{
		"hello":       2,
		"the":         1,
		"beautiful":   3,
		"make":        1,
		"little":      2,
		"HELLO WORLD": 3,
		"it's 2 PM":   2,
		"":            0,
	} {
		if got := prosody.CountSyllables(text, "en"); got != want {
			t.Fatalf("CountSyllables(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestCountSpanishSyllablesKeepsFinalE(t *testing.T) {
	if got := prosody.CountSyllables("buenas noches, hermanos", "es"); got != 7 {
		t.Fatalf("Spanish syllables = %d, want 7", got)
	}
}

func TestTrackerCarriesRelativeDelivery(t *testing.T) {
	tracker := prosody.NewTracker(4)
	first := tracker.Annotate(prosody.Measurement{PitchHz: 150, EnergyDB: -25, Rate: 4, SpeechSeconds: 2, PitchRangeSemitones: 6}, 0)
	if first.RelativePitchSemitones != 0 || first.RelativeEnergyDB != 0 || first.RelativeRate != 1 || len(first.Markers) != 0 {
		t.Fatalf("first utterance defines the baseline: %+v", first)
	}
	near(t, "neutral arousal", float64(first.Arousal), 0.5, 0.01)

	excited := tracker.Annotate(prosody.Measurement{PitchHz: 190, EnergyDB: -18, Rate: 5.6, SpeechSeconds: 2, PitchRangeSemitones: 11, Emphasis: 0.7}, 1200*time.Millisecond)
	for _, marker := range []string{"fast", "loud", "high", "animated", "emphatic", "long-pause"} {
		if !slices.Contains(excited.Markers, marker) {
			t.Fatalf("missing %q in %v", marker, excited.Markers)
		}
	}
	if excited.Arousal < 0.8 || excited.PauseBeforeMillis != 1200 {
		t.Fatalf("unexpected excited prosody: %+v", excited)
	}
	near(t, "relative rate", float64(excited.RelativeRate), 1.4, 0.001)
	near(t, "relative pitch", float64(excited.RelativePitchSemitones), 12*math.Log2(190.0/150), 0.01)
	near(t, "relative range", float64(excited.RelativePitchRangeSemitones), 5, 0.01)

	calm := tracker.Annotate(prosody.Measurement{PitchHz: 120, EnergyDB: -33, Rate: 2.8, SpeechSeconds: 2, PitchRangeSemitones: 2}, 0)
	for _, marker := range []string{"slow", "soft", "low", "monotone"} {
		if !slices.Contains(calm.Markers, marker) {
			t.Fatalf("missing %q in %v", marker, calm.Markers)
		}
	}
	if calm.Arousal > 0.2 {
		t.Fatalf("calm arousal = %.2f", calm.Arousal)
	}
}

func TestTrackerIgnoresShortUtterancesForBaseline(t *testing.T) {
	tracker := prosody.NewTracker(0)
	short := tracker.Annotate(prosody.Measurement{PitchHz: 300, EnergyDB: -10, SpeechSeconds: 0.2}, 0)
	if short.RelativePitchSemitones != 0 || short.RelativeEnergyDB != 0 || short.RelativeRate != 1 {
		t.Fatalf("short utterance must not set relative values without a baseline: %+v", short)
	}
	baseline := tracker.Annotate(prosody.Measurement{PitchHz: 150, EnergyDB: -25, Rate: 4.5, SpeechSeconds: 2}, 0)
	if baseline.RelativePitchSemitones != 0 || baseline.RelativeRate != 1 {
		t.Fatalf("baseline must come from the first usable utterance: %+v", baseline)
	}
}
