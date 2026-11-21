package worker

import (
	"time"

	qol "github.com/aleksclark/qol"
)

const (
	DefaultPlayoutDelay   = time.Second
	DefaultMaxPace        = 1.4
	DefaultCatchUpWindow  = 5 * time.Second
	DefaultMinGap         = 120 * time.Millisecond
	silenceChunkMaxBytes  = 64 * 1024
	synthesisBytesPerTick = 2
)

type Timeline struct {
	Delay         time.Duration
	MaxPace       float64
	CatchUpWindow time.Duration
	MinGap        time.Duration
}

type playoutSlot struct {
	start   qol.MediaTime
	silence time.Duration
	pace    float64
}

func DefaultTimeline() Timeline {
	return Timeline{Delay: DefaultPlayoutDelay, MaxPace: DefaultMaxPace, CatchUpWindow: DefaultCatchUpWindow, MinGap: DefaultMinGap}
}

func (t Timeline) plan(cursor qol.MediaTime, span qol.Span) playoutSlot {
	earliest := span.End + qol.MediaTime(max(t.Delay, 0))
	next := cursor
	if cursor > 0 {
		next += qol.MediaTime(max(t.MinGap, 0))
	}
	slot := playoutSlot{start: max(next, earliest), pace: 1}
	slot.silence = time.Duration(slot.start - cursor)
	if backlog := time.Duration(next - earliest); backlog > 0 && t.MaxPace > 1 {
		window := t.CatchUpWindow
		if window <= 0 {
			window = DefaultCatchUpWindow
		}
		slot.pace = 1 + min(float64(backlog)/float64(window), 1)*(t.MaxPace-1)
	}
	return slot
}

func silencePCM(duration time.Duration, sampleRate int) [][]byte {
	samples := int64(duration) * int64(sampleRate) / int64(time.Second)
	remaining := samples * synthesisBytesPerTick
	var chunks [][]byte
	for remaining > 0 {
		size := min(remaining, silenceChunkMaxBytes)
		chunks = append(chunks, make([]byte, size))
		remaining -= size
	}
	return chunks
}

func pcmDuration(bytes int, sampleRate int) time.Duration {
	return time.Duration(int64(bytes/synthesisBytesPerTick) * int64(time.Second) / int64(sampleRate))
}
