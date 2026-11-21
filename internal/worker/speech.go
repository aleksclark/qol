package worker

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/prosody"
	"github.com/aleksclark/qol/internal/wire"
)

const (
	DefaultSynthesisLookahead = 2 * time.Second
	DefaultFade               = 15 * time.Millisecond
	DefaultProsodySmoothing   = 0.6
	DefaultClauseHold         = 2500 * time.Millisecond
	DefaultSentenceGap        = 350 * time.Millisecond
	DefaultClauseGap          = 180 * time.Millisecond
	DefaultContinuationGap    = 60 * time.Millisecond
	spanishSyllablesPerSecond = 6.5
	maxUnitItems              = 3
	maxUnitRunes              = 280
	trimMargin                = 20 * time.Millisecond
	silenceThreshold          = 100
)

type PhraseGaps struct {
	Sentence     time.Duration
	Clause       time.Duration
	Continuation time.Duration
}

type SpeechOptions struct {
	Timeline    Timeline
	JustInTime  bool
	ClockStage  string
	Lookahead   time.Duration
	Fade        time.Duration
	Smoothing   float64
	Coalesce    bool
	ClauseHold  time.Duration
	Gaps        PhraseGaps
	TrimSilence bool
}

func DefaultSpeechOptions() SpeechOptions {
	return SpeechOptions{
		Timeline:    DefaultTimeline(),
		JustInTime:  true,
		ClockStage:  "prosody",
		Lookahead:   DefaultSynthesisLookahead,
		Fade:        DefaultFade,
		Smoothing:   DefaultProsodySmoothing,
		Coalesce:    true,
		ClauseHold:  DefaultClauseHold,
		Gaps:        PhraseGaps{Sentence: DefaultSentenceGap, Clause: DefaultClauseGap, Continuation: DefaultContinuationGap},
		TrimSilence: true,
	}
}

type speechUnit struct {
	items   []*speechItem
	event   qol.Event
	text    string
	prosody *qol.Prosody
}

type speechItem struct {
	sequence uint64
	event    qol.Event
	text     string
	prosody  *qol.Prosody
}

type speechSession struct {
	id           string
	next         uint64
	outSeq       uint64
	telemetrySeq uint64
	cursor       qol.MediaTime
	clock        qol.MediaTime
	clockKnown   bool
	queue        []*speechItem
	final        *qol.Event
	smoothed     *qol.Prosody
	wake         chan struct{}
	gap          time.Duration
	holding      bool
	holdStart    qol.MediaTime
}

type SpeechStage struct {
	spec           qol.StageSpec
	process        *persistentProcess
	options        SpeechOptions
	now            func() time.Time
	processContext context.Context
	publisher      qol.Publisher
	mu             sync.Mutex
	sessions       map[string]*speechSession
	finished       map[string]time.Time
	workers        sync.WaitGroup
}

var speechFormat = qol.PCMFormat{SampleRate: SynthesisSampleRate, Channels: 1, Format: qol.SampleS16LE}

func NewTTS(processor LiveProcessor, options SpeechOptions) *SpeechStage {
	if options.Lookahead <= 0 {
		options.Lookahead = DefaultSynthesisLookahead
	}
	if options.Smoothing <= 0 || options.Smoothing > 1 {
		options.Smoothing = 1
	}
	inputs := []qol.Port{{Name: "input", Channel: qol.ChannelTranslationText, Types: []string{qol.TypeTranslationFinal}}}
	if options.JustInTime {
		inputs = append(inputs, qol.Port{Name: "clock", Channel: qol.ChannelTelemetry, Types: []string{qol.TypeTelemetry}})
	}
	return &SpeechStage{
		spec: qol.StageSpec{
			Name:    "tts",
			Inputs:  inputs,
			Outputs: []qol.Port{{Name: "output", Channel: qol.ChannelTTSAudio, Types: []string{qol.TypeAudioPCM, qol.TypeAudioPCMFinal}}, {Name: "telemetry", Channel: qol.ChannelTelemetry, Types: []string{qol.TypeTelemetry}}},
		},
		process: newPersistentProcess(processor), options: options, now: time.Now, sessions: make(map[string]*speechSession), finished: make(map[string]time.Time),
	}
}

func (s *SpeechStage) Spec() qol.StageSpec {
	return s.spec
}

func (s *SpeechStage) Run(ctx context.Context, bus qol.Bus) error {
	s.processContext, s.publisher = ctx, bus
	defer s.process.Close()
	defer s.workers.Wait()
	input, err := bus.Subscribe(ctx, s.spec.Inputs[0].Channel, qol.SubscribeOptions{Group: "qol-tts"}, s.handleTranslation)
	if err != nil {
		return err
	}
	defer input.Close()
	var clockDone <-chan struct{}
	if s.options.JustInTime {
		clock, err := bus.Subscribe(ctx, qol.ChannelTelemetry, qol.SubscribeOptions{}, s.handleTelemetry)
		if err != nil {
			return err
		}
		defer clock.Close()
		clockDone = clock.Done()
	}
	select {
	case <-ctx.Done():
		return nil
	case <-input.Done():
		return input.Err()
	case <-clockDone:
		return nil
	}
}

func (s *SpeechStage) sessionLocked(id string) *speechSession {
	session := s.sessions[id]
	if session == nil {
		session = &speechSession{id: id, wake: make(chan struct{}, 1)}
		s.sessions[id] = session
	}
	return session
}

func (s *SpeechStage) handleTelemetry(_ context.Context, event qol.Event) error {
	if event.Type != qol.TypeTelemetry {
		return nil
	}
	telemetry, err := wire.UnmarshalTelemetry(event.Payload)
	if err != nil || telemetry.Stage != s.options.ClockStage {
		return err
	}
	s.mu.Lock()
	if _, done := s.finished[event.SessionID]; done {
		s.mu.Unlock()
		return nil
	}
	session := s.sessionLocked(event.SessionID)
	if !session.clockKnown || telemetry.ConsumedThrough > session.clock {
		session.clock, session.clockKnown = telemetry.ConsumedThrough, true
	}
	s.mu.Unlock()
	s.signal(session)
	return nil
}

func (s *SpeechStage) handleTranslation(_ context.Context, event qol.Event) error {
	if event.Type != qol.TypeTranslationFinal {
		return errors.New("tts requires final translation")
	}
	translation, err := wire.UnmarshalTranslation(event.Payload)
	if err != nil {
		return err
	}
	if translation.TargetLanguage != "es" {
		return errors.New("tts requires Spanish translation")
	}
	s.mu.Lock()
	session := s.sessionLocked(event.SessionID)
	if event.Seq < session.next {
		s.mu.Unlock()
		return nil
	}
	if event.Seq != session.next {
		s.mu.Unlock()
		return fmt.Errorf("tts expected sequence %d, got %d", session.next, event.Seq)
	}
	start := session.next == 0 && session.final == nil && len(session.queue) == 0 && session.outSeq == 0
	session.next++
	switch {
	case translation.Final:
		if text := strings.TrimSpace(translation.Text); text != "" {
			session.queue = append(session.queue, &speechItem{sequence: event.Seq, event: event, text: text, prosody: translation.Prosody})
		}
		final := event
		session.final = &final
	case len(translation.Replaces) > 0:
		s.replaceLocked(session, event, translation)
	case strings.TrimSpace(translation.Text) != "":
		session.queue = append(session.queue, &speechItem{sequence: event.Seq, event: event, text: translation.Text, prosody: translation.Prosody})
	}
	s.mu.Unlock()
	if start {
		s.workers.Add(1)
		go s.speak(session)
	}
	s.signal(session)
	if s.options.JustInTime {
		s.publishTelemetry(session)
	}
	return nil
}

func (s *SpeechStage) replaceLocked(session *speechSession, event qol.Event, translation qol.Translation) {
	positions := make([]int, 0, len(translation.Replaces))
	for _, sequence := range translation.Replaces {
		found := -1
		for index, item := range session.queue {
			if item.sequence == sequence {
				found = index
				break
			}
		}
		if found < 0 {
			slog.Info("tts dropped a stale replacement", "session", session.id, "sequence", event.Seq, "replaces", translation.Replaces)
			return
		}
		positions = append(positions, found)
	}
	first := positions[0]
	for _, position := range positions {
		first = min(first, position)
	}
	span := event.Span
	for _, position := range positions {
		replaced := session.queue[position].event.Span
		span.Start = min(span.Start, replaced.Start)
		span.End = max(span.End, replaced.End)
	}
	replacement := event
	replacement.Span = span
	item := &speechItem{sequence: event.Seq, event: replacement, text: translation.Text, prosody: translation.Prosody}
	remove := make(map[int]bool, len(positions))
	for _, position := range positions {
		remove[position] = true
	}
	queue := make([]*speechItem, 0, len(session.queue))
	for index, existing := range session.queue {
		if index == first {
			queue = append(queue, item)
		}
		if !remove[index] {
			queue = append(queue, existing)
		}
	}
	session.queue = queue
	slog.Info("tts replaced queued speech", "session", session.id, "sequence", event.Seq, "replaces", translation.Replaces)
}

func (s *SpeechStage) signal(session *speechSession) {
	select {
	case session.wake <- struct{}{}:
	default:
	}
}

func (s *SpeechStage) speak(session *speechSession) {
	defer s.workers.Done()
	for {
		item, finished := s.nextDue(session)
		if finished {
			s.finish(session)
			return
		}
		if item == nil {
			select {
			case <-session.wake:
				continue
			case <-s.processContext.Done():
				return
			}
		}
		if err := s.synthesize(session, item); err != nil {
			slog.Error("tts synthesis failed", "session", session.id, "sequence", item.items[0].sequence, "error", err)
		}
		s.publishTelemetry(session)
	}
}

func (s *SpeechStage) nextDue(session *speechSession) (*speechUnit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(session.queue) == 0 {
		return nil, session.final != nil
	}
	head := session.queue[0]
	timeline := s.timelineLocked(session)
	waiting := s.options.JustInTime && session.final == nil
	if waiting {
		slot := timeline.plan(session.cursor, head.event.Span)
		if !session.clockKnown || time.Duration(slot.start-session.clock) > s.options.Lookahead {
			return nil, false
		}
	}
	count := 1
	if s.options.Coalesce {
		runes := len([]rune(head.text))
		for count < len(session.queue) && count < maxUnitItems && !endsClause(session.queue[count-1].text) {
			runes += len([]rune(session.queue[count].text))
			if runes > maxUnitRunes {
				break
			}
			count++
		}
		open := !endsClause(session.queue[count-1].text) && count == len(session.queue) && count < maxUnitItems
		if open && waiting && s.options.ClauseHold > 0 {
			if !session.holding {
				session.holding, session.holdStart = true, session.clock
			}
			if time.Duration(session.clock-session.holdStart) < s.options.ClauseHold {
				return nil, false
			}
		}
	}
	session.holding = false
	items := append([]*speechItem(nil), session.queue[:count]...)
	session.queue = session.queue[count:]
	unit := &speechUnit{items: items, event: items[0].event, prosody: items[len(items)-1].prosody}
	texts := make([]string, 0, len(items))
	for _, item := range items {
		texts = append(texts, strings.TrimSpace(item.text))
		unit.event.Span.Start = min(unit.event.Span.Start, item.event.Span.Start)
		unit.event.Span.End = max(unit.event.Span.End, item.event.Span.End)
		if item.prosody != nil {
			unit.prosody = item.prosody
		}
	}
	unit.text = strings.Join(texts, " ")
	return unit, false
}

func (s *SpeechStage) timelineLocked(session *speechSession) Timeline {
	timeline := s.options.Timeline
	if session.gap > 0 {
		timeline.MinGap = max(timeline.MinGap, session.gap)
	}
	return timeline
}

func (s *SpeechStage) gapAfter(text string) time.Duration {
	trimmed := strings.TrimRight(strings.TrimSpace(text), "\"'»”)]")
	if trimmed == "" {
		return 0
	}
	switch {
	case strings.ContainsRune(".?!…", lastRune(trimmed)):
		return s.options.Gaps.Sentence
	case strings.ContainsRune(",;:", lastRune(trimmed)):
		return s.options.Gaps.Clause
	default:
		return s.options.Gaps.Continuation
	}
}

func endsClause(text string) bool {
	trimmed := strings.TrimRight(strings.TrimSpace(text), "\"'»”)]")
	return trimmed != "" && strings.ContainsRune(".?!…,;:", lastRune(trimmed))
}

func lastRune(text string) rune {
	runes := []rune(text)
	return runes[len(runes)-1]
}

func (s *SpeechStage) synthesize(session *speechSession, item *speechUnit) error {
	s.mu.Lock()
	slot := s.timelineLocked(session).plan(session.cursor, item.event.Span)
	prosody := s.smoothLocked(session, item.prosody)
	s.mu.Unlock()
	for _, silence := range silencePCM(slot.silence, SynthesisSampleRate) {
		if err := s.emit(session, item.event, qol.TypeAudioPCM, silence); err != nil {
			return err
		}
	}
	started := time.Now()
	produced := 0
	var held []byte
	first := true
	fadeSamples := int(int64(s.options.Fade) * SynthesisSampleRate / int64(time.Second))
	margin := int(int64(trimMargin) * SynthesisSampleRate / int64(time.Second))
	flush := func(data []byte, last bool) error {
		if first {
			if s.options.TrimSilence {
				data = trimLeading(data, margin)
			}
			fadeIn(data, fadeSamples)
			first = false
		}
		if last {
			if s.options.TrimSilence {
				data = trimTrailing(data, margin)
			}
			fadeOut(data, fadeSamples)
		}
		if len(data) == 0 {
			return nil
		}
		produced += len(data)
		return s.emit(session, item.event, qol.TypeAudioPCM, data)
	}
	err := s.process.exchange(s.processContext, func(writer io.Writer) error {
		text := singleLine(item.text)
		if s.options.Coalesce && !endsClause(text) {
			text += ","
		}
		return wire.WriteSynthesisRequest(writer, qol.SynthesisRequest{Text: text, Language: "es", Prosody: prosody, Pace: float32(slot.pace)})
	}, func(reader *bufio.Reader) error {
		for {
			chunk, err := wire.ReadSynthesisChunk(reader)
			if err != nil {
				return err
			}
			if chunk.Audio.Format != speechFormat {
				return fmt.Errorf("synthesizer must emit %d Hz mono S16LE, got %+v", SynthesisSampleRate, chunk.Audio.Format)
			}
			if len(chunk.Audio.Data) > 0 {
				if held != nil {
					if err := flush(held, false); err != nil {
						return err
					}
				}
				held = append([]byte(nil), chunk.Audio.Data...)
			}
			if chunk.EndOfUtterance {
				if held != nil {
					return flush(held, true)
				}
				return nil
			}
		}
	})
	if err != nil {
		return fmt.Errorf("tts: %w", err)
	}
	s.mu.Lock()
	session.cursor = slot.start + qol.MediaTime(pcmDuration(produced, SynthesisSampleRate))
	session.gap = s.gapAfter(item.text)
	s.mu.Unlock()
	slog.Info("tts scheduled utterance", "session", session.id, "sequence", item.items[0].sequence, "fragments", len(item.items), "lag", time.Duration(slot.start-item.event.Span.Start).Round(time.Millisecond), "silence", slot.silence.Round(time.Millisecond), "pace", slot.pace, "synthesis", time.Since(started).Round(time.Millisecond))
	return nil
}

func (s *SpeechStage) smoothLocked(session *speechSession, value *qol.Prosody) *qol.Prosody {
	if value == nil {
		return nil
	}
	current := *value
	if session.smoothed != nil && s.options.Smoothing < 1 {
		weight := float32(s.options.Smoothing)
		blend := func(previous, next float32) float32 { return previous + weight*(next-previous) }
		current.RelativeRate = blend(session.smoothed.RelativeRate, current.RelativeRate)
		current.RelativePitchSemitones = blend(session.smoothed.RelativePitchSemitones, current.RelativePitchSemitones)
		current.RelativePitchRangeSemitones = blend(session.smoothed.RelativePitchRangeSemitones, current.RelativePitchRangeSemitones)
		current.RelativeEnergyDB = blend(session.smoothed.RelativeEnergyDB, current.RelativeEnergyDB)
		current.Arousal = blend(session.smoothed.Arousal, current.Arousal)
	}
	stored := current
	session.smoothed = &stored
	return &current
}

func (s *SpeechStage) emit(session *speechSession, source qol.Event, eventType string, data []byte) error {
	payload, err := wire.MarshalAudio(qol.Audio{Format: speechFormat, Data: data})
	if err != nil {
		return err
	}
	s.mu.Lock()
	sequence := session.outSeq
	session.outSeq++
	s.mu.Unlock()
	return s.publisher.Publish(s.processContext, qol.Event{
		ID:         eventID(s.spec.Name, session.id, sequence),
		SessionID:  session.id,
		Channel:    qol.ChannelTTSAudio,
		Type:       eventType,
		Seq:        sequence,
		Span:       source.Span,
		Parents:    []string{source.ID},
		ProducedAt: s.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	})
}

func (s *SpeechStage) finish(session *speechSession) {
	s.mu.Lock()
	final := *session.final
	delete(s.sessions, session.id)
	now := s.now()
	for id, at := range s.finished {
		if now.Sub(at) > 10*time.Minute {
			delete(s.finished, id)
		}
	}
	s.finished[session.id] = now
	s.mu.Unlock()
	if err := s.emit(session, final, qol.TypeAudioPCMFinal, []byte{0, 0}); err != nil {
		slog.Error("tts could not finish stream", "session", session.id, "error", err)
	}
	s.publishTelemetry(session)
}

func (s *SpeechStage) publishTelemetry(session *speechSession) {
	s.mu.Lock()
	telemetry := qol.Telemetry{Stage: s.spec.Name, ConsumedThrough: max(session.clock, 0), OutputCursor: session.cursor}
	end := time.Duration(session.cursor)
	var last *speechItem
	for _, item := range session.queue {
		telemetry.QueuedSequences = append(telemetry.QueuedSequences, item.sequence)
		seconds := float64(prosody.CountSyllables(item.text, "es")) / spanishSyllablesPerSecond
		telemetry.BacklogSeconds += seconds
		end += time.Duration(seconds * float64(time.Second))
		last = item
	}
	if last != nil {
		end = max(end, time.Duration(last.event.Span.End+qol.MediaTime(s.options.Timeline.Delay)))
		telemetry.ProjectedLagSeconds = (end - time.Duration(last.event.Span.Start)).Seconds()
	}
	sequence := session.telemetrySeq
	session.telemetrySeq++
	s.mu.Unlock()
	payload, err := wire.MarshalTelemetry(telemetry)
	if err != nil {
		slog.Error("tts telemetry invalid", "error", err)
		return
	}
	_ = s.publisher.Publish(s.processContext, qol.Event{
		ID:         eventID(s.spec.Name+".telemetry", session.id, sequence),
		SessionID:  session.id,
		Channel:    qol.ChannelTelemetry,
		Type:       qol.TypeTelemetry,
		Seq:        sequence,
		Span:       qol.Span{Start: telemetry.OutputCursor, End: telemetry.OutputCursor},
		ProducedAt: s.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	})
}

func trimLeading(data []byte, margin int) []byte {
	total := len(data) / 2
	first := 0
	for first < total && abs16(data, first) < silenceThreshold {
		first++
	}
	if first == total {
		return data
	}
	return data[max(first-margin, 0)*2:]
}

func trimTrailing(data []byte, margin int) []byte {
	last := len(data)/2 - 1
	for last >= 0 && abs16(data, last) < silenceThreshold {
		last--
	}
	if last < 0 {
		return data
	}
	return data[:min(last+1+margin, len(data)/2)*2]
}

func abs16(data []byte, index int) int {
	value := int(int16(binary.LittleEndian.Uint16(data[index*2:])))
	if value < 0 {
		return -value
	}
	return value
}

func fadeIn(data []byte, samples int) {
	count := min(samples, len(data)/2)
	for index := range count {
		scaleSample(data, index, float64(index)/float64(count))
	}
}

func fadeOut(data []byte, samples int) {
	total := len(data) / 2
	count := min(samples, total)
	for index := range count {
		scaleSample(data, total-1-index, float64(index)/float64(count))
	}
}

func scaleSample(data []byte, index int, factor float64) {
	value := int16(binary.LittleEndian.Uint16(data[index*2:]))
	binary.LittleEndian.PutUint16(data[index*2:], uint16(int16(float64(value)*factor)))
}

var _ qol.Stage = (*SpeechStage)(nil)
