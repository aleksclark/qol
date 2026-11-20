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
	ProsodySampleRate     = 16000
	DefaultProsodyTimeout = 5 * time.Second
)

type prosodySession struct {
	audioNext uint64
	input     io.WriteCloser
	cancel    context.CancelFunc
	samples   []int16
	dropped   int64
	audioDone bool
	changed   chan struct{}

	textNext      uint64
	textDone      bool
	tracker       *prosody.Tracker
	lastSpeechEnd qol.MediaTime
	spoken        bool
}

type ProsodyStage struct {
	spec           qol.StageSpec
	decoder        LiveProcessor
	emotion        *persistentProcess
	attachAudio    bool
	referenceRate  float64
	timeout        time.Duration
	now            func() time.Time
	processContext context.Context
	mu             sync.Mutex
	sessions       map[string]*prosodySession
}

func NewProsody(decoder, emotion LiveProcessor, referenceRate float64, timeout time.Duration) *ProsodyStage {
	if timeout <= 0 {
		timeout = DefaultProsodyTimeout
	}
	var classifier *persistentProcess
	if emotion != nil {
		classifier = newPersistentProcess(emotion)
	}
	return &ProsodyStage{
		spec: qol.StageSpec{
			Name: "prosody",
			Inputs: []qol.Port{
				{Name: "utterances", Channel: qol.ChannelUtterances, Types: []string{qol.TypeTextFinal}},
				{Name: "audio", Channel: qol.ChannelProsodyAudio, Types: []string{qol.TypeAudioStream}},
			},
			Outputs: []qol.Port{{Name: "output", Channel: qol.ChannelASRText, Types: []string{qol.TypeTextFinal}}},
		},
		decoder: decoder, emotion: classifier, referenceRate: referenceRate, timeout: timeout, now: time.Now, sessions: make(map[string]*prosodySession),
	}
}

func (s *ProsodyStage) AttachReferenceAudio(enabled bool) *ProsodyStage {
	s.attachAudio = enabled
	return s
}

func (s *ProsodyStage) Spec() qol.StageSpec {
	return s.spec
}

func (s *ProsodyStage) Run(ctx context.Context, bus qol.Bus) error {
	s.processContext = ctx
	if s.emotion != nil {
		defer s.emotion.Close()
	}
	text, err := bus.Subscribe(ctx, s.spec.Inputs[0].Channel, qol.SubscribeOptions{Group: "qol-prosody"}, func(eventContext context.Context, event qol.Event) error {
		return s.handleText(eventContext, bus, event)
	})
	if err != nil {
		return err
	}
	defer text.Close()
	audio, err := bus.Subscribe(ctx, s.spec.Inputs[1].Channel, qol.SubscribeOptions{Group: "qol-prosody"}, s.handleAudio)
	if err != nil {
		return err
	}
	defer audio.Close()
	select {
	case <-ctx.Done():
		return nil
	case <-text.Done():
		return text.Err()
	case <-audio.Done():
		return audio.Err()
	}
}

func (s *ProsodyStage) session(sessionID string) *prosodySession {
	session := s.sessions[sessionID]
	if session == nil {
		session = &prosodySession{changed: make(chan struct{}), tracker: prosody.NewTracker(s.referenceRate)}
		s.sessions[sessionID] = session
	}
	return session
}

func (s *ProsodyStage) handleAudio(ctx context.Context, event qol.Event) error {
	value, err := decodeAudioStream(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	session := s.session(event.SessionID)
	if event.Seq < session.audioNext {
		s.mu.Unlock()
		return nil
	}
	if event.Seq != session.audioNext {
		s.mu.Unlock()
		return fmt.Errorf("prosody audio expected sequence %d, got %d", session.audioNext, event.Seq)
	}
	if event.Seq == 0 {
		if err := s.startDecoder(ctx, event.SessionID, session); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	session.audioNext++
	input := session.input
	s.mu.Unlock()
	if len(value.data) > 0 {
		if _, err := input.Write(value.data); err != nil {
			return fmt.Errorf("decode prosody audio: %w", err)
		}
	}
	if value.end {
		return input.Close()
	}
	return nil
}

func (s *ProsodyStage) startDecoder(ctx context.Context, sessionID string, session *prosodySession) error {
	processContext := s.processContext
	if processContext == nil {
		processContext = ctx
	}
	processContext, cancel := context.WithCancel(processContext)
	input, output, wait, err := s.decoder.Start(processContext)
	if err != nil {
		cancel()
		return fmt.Errorf("start prosody decoder: %w", err)
	}
	session.input, session.cancel = input, cancel
	go s.readPCM(sessionID, session, output, wait)
	return nil
}

func (s *ProsodyStage) readPCM(sessionID string, session *prosodySession, output io.ReadCloser, wait func() error) {
	defer output.Close()
	buffer := make([]byte, 8192)
	var carry []byte
	for {
		size, err := output.Read(buffer)
		if size > 0 {
			data := append(carry, buffer[:size]...)
			whole := len(data) / 2 * 2
			s.mu.Lock()
			for index := 0; index < whole; index += 2 {
				session.samples = append(session.samples, int16(uint16(data[index])|uint16(data[index+1])<<8))
			}
			s.notify(session)
			s.mu.Unlock()
			carry = append([]byte(nil), data[whole:]...)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Warn("prosody decoder read failed", "session", sessionID, "error", err)
			}
			break
		}
	}
	if err := wait(); err != nil {
		slog.Warn("prosody decoder exited", "session", sessionID, "error", err)
	}
	s.mu.Lock()
	session.audioDone = true
	s.notify(session)
	s.release(sessionID, session)
	s.mu.Unlock()
}

func (s *ProsodyStage) notify(session *prosodySession) {
	close(session.changed)
	session.changed = make(chan struct{})
}

func (s *ProsodyStage) release(sessionID string, session *prosodySession) {
	if session.audioDone && session.textDone && s.sessions[sessionID] == session {
		delete(s.sessions, sessionID)
		if session.cancel != nil {
			session.cancel()
		}
	}
}

func (s *ProsodyStage) handleText(ctx context.Context, publisher qol.Publisher, event qol.Event) error {
	if event.Type != qol.TypeTextFinal {
		return errors.New("prosody requires final text")
	}
	text, err := wire.UnmarshalText(event.Payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	session := s.session(event.SessionID)
	if event.Seq < session.textNext {
		s.mu.Unlock()
		return nil
	}
	if event.Seq != session.textNext {
		s.mu.Unlock()
		return fmt.Errorf("prosody text expected sequence %d, got %d", session.textNext, event.Seq)
	}
	session.textNext++
	s.mu.Unlock()
	if strings.TrimSpace(text.Text) != "" {
		annotated, err := s.annotate(ctx, event, session, text.Text)
		if err != nil {
			return err
		}
		text.Prosody = annotated
	}
	if text.Final {
		s.mu.Lock()
		session.textDone = true
		s.release(event.SessionID, session)
		s.mu.Unlock()
	}
	payload, err := wire.MarshalText(text)
	if err != nil {
		return err
	}
	return publisher.Publish(ctx, qol.Event{
		ID:         eventID(s.spec.Name, event.SessionID, event.Seq),
		SessionID:  event.SessionID,
		Channel:    s.spec.Outputs[0].Channel,
		Type:       qol.TypeTextFinal,
		Seq:        event.Seq,
		Span:       event.Span,
		Parents:    []string{event.ID},
		ProducedAt: s.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	})
}

func (s *ProsodyStage) annotate(ctx context.Context, event qol.Event, session *prosodySession, text string) (*qol.Prosody, error) {
	start := sampleIndex(event.Span.Start)
	end := sampleIndex(event.Span.End)
	segment, ok, err := s.segment(ctx, session, start, end)
	if err != nil {
		return nil, err
	}
	if !ok {
		slog.Warn("prosody audio unavailable; forwarding text without prosody", "session", event.SessionID, "sequence", event.Seq, "span_end", event.Span.End)
		return nil, nil
	}
	measurement := prosody.Analyze(segment, ProsodySampleRate, prosody.CountSyllables(text, "en"))
	if measurement.SpeechSeconds == 0 {
		return nil, nil
	}
	speechStart := event.Span.Start + qol.MediaTime(measurement.LeadingSilence)
	s.mu.Lock()
	var pause time.Duration
	if session.spoken && speechStart > session.lastSpeechEnd {
		pause = time.Duration(speechStart - session.lastSpeechEnd)
	}
	session.lastSpeechEnd = event.Span.Start + qol.MediaTime(measurement.SpeechEnd)
	session.spoken = true
	result := session.tracker.Annotate(measurement, pause)
	s.mu.Unlock()
	if s.attachAudio {
		reference := pcmBytes(segment)
		if len(reference) > wire.MaxReferenceBytes {
			reference = reference[:wire.MaxReferenceBytes]
		}
		result.ReferenceAudio = &qol.Audio{Format: qol.PCMFormat{SampleRate: ProsodySampleRate, Channels: 1, Format: qol.SampleS16LE}, Data: reference}
	}
	if s.emotion != nil {
		emotion, err := s.classify(segment)
		if err != nil {
			slog.Warn("emotion classifier failed; forwarding prosody without emotion", "session", event.SessionID, "sequence", event.Seq, "error", err)
		} else {
			result.Emotion, result.EmotionConfidence, result.EmotionScores = emotion.Label, emotion.Confidence, emotion.Scores
			if emotion.Label != "neutral" && emotion.Confidence >= 0.5 {
				result.Markers = append(result.Markers, emotion.Label)
			}
		}
	}
	if err := wire.ValidateProsody(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *ProsodyStage) segment(ctx context.Context, session *prosodySession, start, end int64) ([]int16, bool, error) {
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for session.dropped+int64(len(session.samples)) < end && !session.audioDone {
		changed := session.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			s.mu.Lock()
			return nil, false, nil
		case <-ctx.Done():
			s.mu.Lock()
			return nil, false, ctx.Err()
		}
		s.mu.Lock()
	}
	available := session.dropped + int64(len(session.samples))
	start = max(start, session.dropped)
	end = min(end, available)
	if end <= start {
		return nil, false, nil
	}
	segment := append([]int16(nil), session.samples[start-session.dropped:end-session.dropped]...)
	keep := end - session.dropped
	session.samples = append([]int16(nil), session.samples[keep:]...)
	session.dropped = end
	return segment, true, nil
}

func pcmBytes(samples []int16) []byte {
	data := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(data[index*2:], uint16(sample))
	}
	return data
}

func (s *ProsodyStage) classify(segment []int16) (qol.EmotionResult, error) {
	audio := qol.Audio{Format: qol.PCMFormat{SampleRate: ProsodySampleRate, Channels: 1, Format: qol.SampleS16LE}, Data: pcmBytes(segment)}
	processContext := s.processContext
	if processContext == nil {
		processContext = context.Background()
	}
	var result qol.EmotionResult
	err := s.emotion.exchange(processContext, func(writer io.Writer) error {
		return wire.WriteEmotionRequest(writer, audio)
	}, func(reader *bufio.Reader) error {
		var err error
		result, err = wire.ReadEmotionResult(reader)
		return err
	})
	return result, err
}

func sampleIndex(at qol.MediaTime) int64 {
	if at <= 0 {
		return 0
	}
	return int64(at) * ProsodySampleRate / int64(time.Second)
}

var _ qol.Stage = (*ProsodyStage)(nil)
