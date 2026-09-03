package media

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	qol "github.com/aleksclark/qol"
)

const (
	OpusClockRate      = 48000
	OpusFrameMs        = 20
	OpusFrameSamples   = OpusClockRate * OpusFrameMs / 1000
	MaxOpusChannels    = 2
	DefaultPayloadType = 111
	MimeTypeOpus       = "audio/opus"
	MediaTypeOggOpus   = "audio/ogg; codecs=opus"
)

type ProfileKind string

const (
	ProfilePCMS16LE ProfileKind = "pcm-s16le"
	ProfilePCMF32LE ProfileKind = "pcm-f32le"
	ProfileOggOpus  ProfileKind = "ogg-opus"
)

type OverflowPolicy int

const (
	OverflowDropOldest OverflowPolicy = iota
	OverflowFail
)

type Codec struct {
	MimeType  string
	ClockRate uint32
	Channels  uint16
}

type Profile struct {
	Kind       ProfileKind
	SampleRate int
	Channels   int
}

type Frame struct {
	Payload     []byte
	Sequence    uint16
	Timestamp   uint32
	Marker      bool
	PayloadType uint8
	SSRC        uint32
	End         bool
}

type OutputChunk struct {
	Data []byte
	Span qol.Span
	End  bool
}

type Metrics struct {
	FramesIn        atomic.Uint64
	FramesOut       atomic.Uint64
	Dropped         atomic.Uint64
	LatePackets     atomic.Uint64
	Discontinuities atomic.Uint64
	Resets          atomic.Uint64
	ProcessExits    atomic.Uint64
}

var (
	ErrUnsupportedCodec = errors.New("unsupported negotiated codec")
	ErrUnsupportedMedia = errors.New("unsupported media format")
	ErrFormatChanged    = errors.New("media format changed mid-stream")
	ErrOverflow         = errors.New("media queue overflow")
	ErrClosed           = errors.New("converter closed")
	ErrMalformedMedia   = errors.New("malformed media")
)

func ValidateCodec(codec Codec) error {
	if !isOpusMIME(codec.MimeType) {
		return fmt.Errorf("%w: %s clock=%d channels=%d", ErrUnsupportedCodec, codec.MimeType, codec.ClockRate, codec.Channels)
	}
	if codec.ClockRate != 0 && codec.ClockRate != OpusClockRate {
		return fmt.Errorf("%w: %s clock=%d channels=%d", ErrUnsupportedCodec, codec.MimeType, codec.ClockRate, codec.Channels)
	}
	channels := int(codec.Channels)
	if channels == 0 {
		channels = 1
	}
	if channels < 1 || channels > MaxOpusChannels {
		return fmt.Errorf("%w: %s clock=%d channels=%d", ErrUnsupportedCodec, codec.MimeType, codec.ClockRate, codec.Channels)
	}
	return nil
}

func ValidateProfile(profile Profile) error {
	switch profile.Kind {
	case ProfilePCMS16LE, ProfilePCMF32LE:
		if profile.SampleRate <= 0 || profile.Channels <= 0 {
			return fmt.Errorf("pcm profile requires rate and channels")
		}
		return nil
	case ProfileOggOpus:
		return nil
	default:
		return fmt.Errorf("unsupported output profile %q", profile.Kind)
	}
}

func ParseProfile(kind string, rate, channels int) (Profile, error) {
	profile := Profile{Kind: ProfileKind(strings.ToLower(strings.TrimSpace(kind))), SampleRate: rate, Channels: channels}
	if profile.Kind == "" {
		profile.Kind = ProfilePCMS16LE
	}
	if (profile.Kind == ProfilePCMS16LE || profile.Kind == ProfilePCMF32LE) && profile.SampleRate == 0 {
		profile.SampleRate = 16000
	}
	if (profile.Kind == ProfilePCMS16LE || profile.Kind == ProfilePCMF32LE) && profile.Channels == 0 {
		profile.Channels = 1
	}
	return profile, ValidateProfile(profile)
}

func (p Profile) SampleFormat() qol.SampleFormat {
	if p.Kind == ProfilePCMF32LE {
		return qol.SampleF32LE
	}
	return qol.SampleS16LE
}

func (p Profile) PCMFormat() qol.PCMFormat {
	return qol.PCMFormat{SampleRate: p.SampleRate, Channels: p.Channels, Format: p.SampleFormat()}
}

func (p Profile) EventType(end bool) string {
	if p.Kind == ProfileOggOpus {
		return qol.TypeAudioStream
	}
	if end {
		return qol.TypeAudioPCMFinal
	}
	return qol.TypeAudioPCM
}

func BytesPerSample(format qol.SampleFormat) int {
	if format == qol.SampleF32LE {
		return 4
	}
	return 2
}

func PCMByteSize(format qol.PCMFormat, frames int) int {
	return frames * format.Channels * BytesPerSample(format.Format)
}

func SpanFromSamples(start qol.MediaTime, samples, rate int) qol.Span {
	if rate <= 0 {
		rate = OpusClockRate
	}
	duration := time.Duration(samples) * time.Second / time.Duration(rate)
	return qol.Span{Start: start, End: start + qol.MediaTime(duration)}
}

func RTPDuration(delta, clockRate uint32) time.Duration {
	if clockRate == 0 {
		clockRate = OpusClockRate
	}
	return time.Duration(delta) * time.Second / time.Duration(clockRate)
}

func SamePCMFormat(a, b qol.PCMFormat) bool {
	return a.SampleRate == b.SampleRate && a.Channels == b.Channels && a.Format == b.Format
}

func isOpusMIME(mime string) bool {
	value := strings.ToLower(strings.TrimSpace(mime))
	return value == MimeTypeOpus || value == "opus" || strings.HasSuffix(value, "/opus")
}
