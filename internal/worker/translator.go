package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

type translatorItem struct {
	turns   []uint64
	span    qol.Span
	prosody *qol.Prosody
	parents []string
}

func (i *translatorItem) absorb(turn uint64, event qol.Event, prosody *qol.Prosody) {
	if len(i.turns) == 0 {
		i.span = event.Span
	} else {
		i.span.Start = min(i.span.Start, event.Span.Start)
		i.span.End = max(i.span.End, event.Span.End)
	}
	i.turns = append(i.turns, turn)
	i.parents = append(i.parents, event.ID)
	if prosody != nil {
		i.prosody = prosody
	}
}

type translatorSession struct {
	mu           sync.Mutex
	next         uint64
	outSeq       uint64
	telemetrySeq uint64
	pending      *translatorItem
	items        map[uint64]*translatorItem
	condensed    map[uint64]bool
}

type TranslatorStage struct {
	spec      qol.StageSpec
	process   *persistentProcess
	now       func() time.Time
	publisher qol.Publisher
	ctx       context.Context
	mu        sync.Mutex
	sessions  map[string]*translatorSession
}

func NewStreamingTranslate(processor LiveProcessor) *TranslatorStage {
	return &TranslatorStage{
		spec: qol.StageSpec{
			Name: "translate",
			Inputs: []qol.Port{
				{Name: "input", Channel: qol.ChannelASRText, Types: []string{qol.TypeTextFinal}},
				{Name: "control", Channel: qol.ChannelTranslateControl, Types: []string{qol.TypeControl}},
			},
			Outputs: []qol.Port{
				{Name: "output", Channel: qol.ChannelTranslationText, Types: []string{qol.TypeTranslationFinal}},
				{Name: "telemetry", Channel: qol.ChannelTelemetry, Types: []string{qol.TypeTelemetry}},
			},
		},
		process: newPersistentProcess(processor), now: time.Now, sessions: make(map[string]*translatorSession),
	}
}

func (s *TranslatorStage) Spec() qol.StageSpec {
	return s.spec
}

func (s *TranslatorStage) Run(ctx context.Context, bus qol.Bus) error {
	s.ctx, s.publisher = ctx, bus
	defer s.process.Close()
	text, err := bus.Subscribe(ctx, qol.ChannelASRText, qol.SubscribeOptions{Group: "qol-translate"}, s.handleText)
	if err != nil {
		return err
	}
	defer text.Close()
	control, err := bus.Subscribe(ctx, qol.ChannelTranslateControl, qol.SubscribeOptions{Group: "qol-translate"}, s.handleControl)
	if err != nil {
		return err
	}
	defer control.Close()
	select {
	case <-ctx.Done():
		return nil
	case <-text.Done():
		return text.Err()
	case <-control.Done():
		return control.Err()
	}
}

func (s *TranslatorStage) handleText(_ context.Context, event qol.Event) error {
	input, err := decodeText(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	session := s.sessions[event.SessionID]
	if session == nil {
		if event.Seq != 0 {
			s.mu.Unlock()
			return fmt.Errorf("translate stream starts at sequence %d", event.Seq)
		}
		session = &translatorSession{items: make(map[uint64]*translatorItem), condensed: make(map[uint64]bool)}
		s.sessions[event.SessionID] = session
	}
	s.mu.Unlock()
	session.mu.Lock()
	defer session.mu.Unlock()
	if event.Seq < session.next {
		return nil
	}
	if event.Seq != session.next {
		return fmt.Errorf("translate expected sequence %d, got %d", session.next, event.Seq)
	}
	session.next++
	text := singleLine(input.text)
	if text != "" || (input.final && session.pending != nil) {
		if session.pending == nil {
			session.pending = &translatorItem{}
		}
		session.pending.absorb(event.Seq, event, input.prosody)
		response, err := s.request(event.SessionID, session, qol.TranslatorRequest{SessionID: event.SessionID, Kind: qol.TranslatorTurn, TurnID: event.Seq, Text: text, Final: input.final})
		if err != nil {
			return err
		}
		if spanish := strings.TrimSpace(response.Text); spanish != "" {
			item := session.pending
			session.pending = nil
			if err := s.publish(event.SessionID, session, item, qol.Translation{Text: spanish, Prosody: item.prosody}); err != nil {
				return err
			}
		}
	}
	if !input.final {
		return nil
	}
	if _, err := s.request(event.SessionID, session, qol.TranslatorRequest{SessionID: event.SessionID, Kind: qol.TranslatorEnd}); err != nil {
		slog.Warn("translator could not close session context", "session", event.SessionID, "error", err)
	}
	s.mu.Lock()
	delete(s.sessions, event.SessionID)
	s.mu.Unlock()
	return s.publish(event.SessionID, session, &translatorItem{span: qol.Span{Start: event.Span.End, End: event.Span.End}, parents: []string{event.ID}}, qol.Translation{Text: " ", Final: true})
}

func (s *TranslatorStage) handleControl(_ context.Context, event qol.Event) error {
	if event.Type != qol.TypeControl {
		return errors.New("translator control requires a control event")
	}
	control, err := wire.UnmarshalControl(event.Payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	session := s.sessions[event.SessionID]
	s.mu.Unlock()
	if session == nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	switch control.Kind {
	case qol.ControlCondense:
		return s.condense(event, session, control)
	case qol.ControlSummarize:
		_, err := s.request(event.SessionID, session, qol.TranslatorRequest{SessionID: event.SessionID, Kind: qol.TranslatorSummarize})
		return err
	case qol.ControlGlossary:
		_, err := s.request(event.SessionID, session, qol.TranslatorRequest{SessionID: event.SessionID, Kind: qol.TranslatorGlossary, Glossary: control.Glossary})
		return err
	default:
		return fmt.Errorf("unsupported translator control %d", control.Kind)
	}
}

func (s *TranslatorStage) condense(event qol.Event, session *translatorSession, control qol.Control) error {
	merged := &translatorItem{}
	var replaces []uint64
	for _, sequence := range control.TargetSequences {
		item := session.items[sequence]
		if item == nil || session.condensed[sequence] {
			continue
		}
		if len(merged.turns) == 0 {
			merged.span = item.span
		}
		merged.span.Start = min(merged.span.Start, item.span.Start)
		merged.span.End = max(merged.span.End, item.span.End)
		merged.turns = append(merged.turns, item.turns...)
		merged.parents = append(merged.parents, item.parents...)
		if item.prosody != nil {
			merged.prosody = item.prosody
		}
		replaces = append(replaces, sequence)
	}
	if len(replaces) == 0 {
		return nil
	}
	response, err := s.request(event.SessionID, session, qol.TranslatorRequest{SessionID: event.SessionID, Kind: qol.TranslatorCondense, TurnIDs: merged.turns, TargetRatio: control.TargetRatio})
	if err != nil {
		return err
	}
	spanish := strings.TrimSpace(response.Text)
	if spanish == "" {
		return nil
	}
	for _, sequence := range replaces {
		session.condensed[sequence] = true
	}
	merged.parents = append(merged.parents, event.ID)
	slog.Info("translator condensed backlog", "session", event.SessionID, "replaces", replaces, "ratio", control.TargetRatio)
	return s.publish(event.SessionID, session, merged, qol.Translation{Text: spanish, Prosody: merged.prosody, Replaces: replaces})
}

func (s *TranslatorStage) request(sessionID string, session *translatorSession, request qol.TranslatorRequest) (qol.TranslatorResponse, error) {
	started := time.Now()
	var response qol.TranslatorResponse
	err := s.process.exchange(s.ctx, func(writer io.Writer) error {
		return wire.WriteTranslatorRequest(writer, request)
	}, func(reader *bufio.Reader) error {
		var err error
		response, err = wire.ReadTranslatorResponse(reader)
		return err
	})
	if err != nil {
		return qol.TranslatorResponse{}, fmt.Errorf("translator: %w", err)
	}
	s.telemetry(sessionID, session, time.Since(started), response.ContextTurns)
	return response, nil
}

func (s *TranslatorStage) publish(sessionID string, session *translatorSession, item *translatorItem, translation qol.Translation) error {
	translation.SourceLanguage, translation.TargetLanguage = "en", "es"
	payload, err := wire.MarshalTranslation(translation)
	if err != nil {
		return err
	}
	sequence := session.outSeq
	session.outSeq++
	if !translation.Final {
		session.items[sequence] = item
	}
	return s.publisher.Publish(s.ctx, qol.Event{
		ID:         eventID(s.spec.Name, sessionID, sequence),
		SessionID:  sessionID,
		Channel:    qol.ChannelTranslationText,
		Type:       qol.TypeTranslationFinal,
		Seq:        sequence,
		Span:       item.span,
		Parents:    append([]string(nil), item.parents...),
		ProducedAt: s.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	})
}

func (s *TranslatorStage) telemetry(sessionID string, session *translatorSession, elapsed time.Duration, turns uint32) {
	payload, err := wire.MarshalTelemetry(qol.Telemetry{Stage: s.spec.Name, ProcessingMillis: float64(elapsed.Microseconds()) / 1000, ContextTurns: turns})
	if err != nil {
		return
	}
	sequence := session.telemetrySeq
	session.telemetrySeq++
	_ = s.publisher.Publish(s.ctx, qol.Event{
		ID:         eventID(s.spec.Name+".telemetry", sessionID, sequence),
		SessionID:  sessionID,
		Channel:    qol.ChannelTelemetry,
		Type:       qol.TypeTelemetry,
		Seq:        sequence,
		ProducedAt: s.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	})
}

var _ qol.Stage = (*TranslatorStage)(nil)
