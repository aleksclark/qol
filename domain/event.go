package domain

import (
	"context"
	"errors"
	"time"
)

type MediaTime int64

func (t MediaTime) Duration() time.Duration {
	return time.Duration(t)
}

type Span struct {
	Start MediaTime
	End   MediaTime
}

type PCM struct {
	SampleRate uint32
	Channels   uint32
	Frames     uint32
	Data       []byte
}

func (p PCM) Validate() error {
	if p.SampleRate == 0 || p.Channels == 0 {
		return errors.New("sample rate and channels must be positive")
	}
	if uint64(len(p.Data)) != uint64(p.Frames)*uint64(p.Channels)*2 {
		return errors.New("PCM byte count does not match frame count")
	}
	return nil
}

type Event struct {
	ID        string
	SessionID string
	Sequence  uint64
	Span      Span
	Parents   []string
	Produced  time.Time
	PCM       PCM
}

type Publisher interface {
	Publish(context.Context, string, Event) error
}
