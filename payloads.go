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
}

type Prosody struct {
	PitchHz float32
	Energy  float32

	Rate     float32
	Emphasis float32
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
}

type StoredAudio struct {
	MediaType string
	Path      string
	SizeBytes uint64
}
