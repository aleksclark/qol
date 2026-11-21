package qol

type PCMFormat struct {
	SampleRate int
	Channels   int
	Format     SampleFormat
}

type SampleFormat uint8

const (
	SampleS16LE SampleFormat = iota + 1
	SampleF32LE
)

type Audio struct {
	Format PCMFormat
	Data   []byte
}

type Text struct {
	Text string

	Final bool

	Language string
	Speaker  string

	Confidence float32

	Prosody *Prosody
}

type Prosody struct {
	PitchHz  float32
	Energy   float32
	Rate     float32
	Emphasis float32

	PitchRangeSemitones    float32
	RelativePitchSemitones float32
	RelativeEnergyDB       float32
	RelativeRate           float32
	Arousal                float32

	PauseBeforeMillis uint32
	VoicedRatio       float32
	SpeechSeconds     float32

	Emotion           string
	EmotionConfidence float32
	EmotionScores     []EmotionScore

	RelativePitchRangeSemitones float32

	Markers []string

	ReferenceAudio *Audio
}

type EmotionScore struct {
	Label string
	Score float32
}

type EmotionResult struct {
	Label      string
	Confidence float32
	Scores     []EmotionScore
}

type Speaker struct {
	ID         string
	Confidence float32
}

type Translation struct {
	Text string

	SourceLanguage string
	TargetLanguage string

	Final bool

	Prosody *Prosody

	Replaces []uint64
}

type ControlKind uint8

const (
	ControlCondense ControlKind = iota + 1
	ControlSummarize
	ControlGlossary
)

type GlossaryEntry struct {
	Source string
	Target string
}

type Control struct {
	Kind            ControlKind
	TargetSequences []uint64
	TargetRatio     float32
	Glossary        []GlossaryEntry
}

type Telemetry struct {
	Stage               string
	ConsumedThrough     MediaTime
	OutputCursor        MediaTime
	BacklogSeconds      float64
	QueuedSequences     []uint64
	ProjectedLagSeconds float64
	ProcessingMillis    float64
	ContextTurns        uint32
}

type TranslatorRequestKind uint8

const (
	TranslatorTurn TranslatorRequestKind = iota + 1
	TranslatorCondense
	TranslatorSummarize
	TranslatorGlossary
	TranslatorEnd
)

type TranslatorRequest struct {
	SessionID   string
	Kind        TranslatorRequestKind
	TurnID      uint64
	Text        string
	Final       bool
	TurnIDs     []uint64
	TargetRatio float32
	Glossary    []GlossaryEntry
}

type TranslatorResponse struct {
	Text         string
	ContextTurns uint32
}

type StoredAudio struct {
	MediaType string
	Path      string
	SizeBytes uint64
}

type Utterance struct {
	Span Span
	Text string
}

type SynthesisRequest struct {
	Text     string
	Language string
	Prosody  *Prosody
	Pace     float32
}

type SynthesisChunk struct {
	Audio          Audio
	EndOfUtterance bool
}
