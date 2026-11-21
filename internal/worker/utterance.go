package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

const SynthesisSampleRate = 22050

type utteranceInput struct {
	text    string
	prosody *qol.Prosody
	final   bool
}

type emitFunc func(eventType string, payload []byte) error

type utteranceSession struct {
	next   uint64
	outSeq uint64
	cursor qol.MediaTime
}

type UtteranceStage struct {
	spec      qol.StageSpec
	group     string
	process   *persistentProcess
	decode    func(qol.Event) (utteranceInput, error)
	transform func(context.Context, qol.Event, *utteranceSession, utteranceInput, emitFunc) error
	finish    func(emitFunc) error
	now       func() time.Time
	mu        sync.Mutex
	sessions  map[string]*utteranceSession
}

func NewTranslate(processor LiveProcessor) *UtteranceStage {
	stage := &UtteranceStage{
		spec: qol.StageSpec{
			Name:    "translate",
			Inputs:  []qol.Port{{Name: "input", Channel: qol.ChannelASRText, Types: []string{qol.TypeTextFinal}}},
			Outputs: []qol.Port{{Name: "output", Channel: qol.ChannelTranslationText, Types: []string{qol.TypeTranslationFinal}}},
		},
		group: "qol-translate", process: newPersistentProcess(processor), decode: decodeText, now: time.Now, sessions: make(map[string]*utteranceSession),
	}
	stage.transform = func(ctx context.Context, _ qol.Event, _ *utteranceSession, input utteranceInput, emit emitFunc) error {
		var spanish string
		err := stage.process.exchange(ctx, func(writer io.Writer) error {
			_, err := io.WriteString(writer, singleLine(input.text)+"\n")
			return err
		}, func(reader *bufio.Reader) error {
			line, err := reader.ReadString('\n')
			spanish = strings.TrimSpace(line)
			return err
		})
		if err != nil {
			return fmt.Errorf("translate: %w", err)
		}
		if spanish == "" {
			return errors.New("translate produced no text")
		}
		payload, err := wire.MarshalTranslation(qol.Translation{Text: spanish, SourceLanguage: "en", TargetLanguage: "es", Prosody: input.prosody})
		if err != nil {
			return err
		}
		return emit(qol.TypeTranslationFinal, payload)
	}
	stage.finish = func(emit emitFunc) error {
		payload, err := wire.MarshalTranslation(qol.Translation{Text: " ", SourceLanguage: "en", TargetLanguage: "es", Final: true})
		if err != nil {
			return err
		}
		return emit(qol.TypeTranslationFinal, payload)
	}
	return stage
}

func (s *UtteranceStage) Spec() qol.StageSpec {
	return s.spec
}

func (s *UtteranceStage) Run(ctx context.Context, bus qol.Bus) error {
	defer s.process.Close()
	return runSubscription(ctx, bus, s.spec.Inputs[0].Channel, s.group, func(eventContext context.Context, event qol.Event) error {
		return s.handle(ctx, eventContext, bus, event)
	})
}

func (s *UtteranceStage) handle(processContext, ctx context.Context, publisher qol.Publisher, event qol.Event) error {
	input, err := s.decode(event)
	if err != nil {
		return err
	}
	session, skip, err := s.session(event)
	if err != nil || skip {
		return err
	}
	emit := func(eventType string, payload []byte) error {
		sequence := session.outSeq
		session.outSeq++
		return publisher.Publish(ctx, qol.Event{
			ID:         eventID(s.spec.Name, event.SessionID, sequence),
			SessionID:  event.SessionID,
			Channel:    s.spec.Outputs[0].Channel,
			Type:       eventType,
			Seq:        sequence,
			Span:       event.Span,
			Parents:    []string{event.ID},
			ProducedAt: s.now().UTC(),
			Encoding:   qol.EncodingProtobuf,
			Payload:    payload,
		})
	}
	if strings.TrimSpace(input.text) != "" {
		if err := s.transform(processContext, event, session, input, emit); err != nil {
			return err
		}
	}
	if !input.final {
		return nil
	}
	s.mu.Lock()
	delete(s.sessions, event.SessionID)
	s.mu.Unlock()
	return s.finish(emit)
}

func (s *UtteranceStage) session(event qol.Event) (*utteranceSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[event.SessionID]
	if session == nil {
		if event.Seq != 0 {
			return nil, false, fmt.Errorf("%s stream starts at sequence %d", s.spec.Name, event.Seq)
		}
		session = &utteranceSession{}
		s.sessions[event.SessionID] = session
	}
	if event.Seq < session.next {
		return session, true, nil
	}
	if event.Seq != session.next {
		return nil, false, fmt.Errorf("%s expected sequence %d, got %d", s.spec.Name, session.next, event.Seq)
	}
	session.next++
	return session, false, nil
}

func decodeText(event qol.Event) (utteranceInput, error) {
	if event.Type != qol.TypeTextFinal {
		return utteranceInput{}, errors.New("translate requires final text")
	}
	text, err := wire.UnmarshalText(event.Payload)
	if err != nil {
		return utteranceInput{}, err
	}
	if text.Language != "en" {
		return utteranceInput{}, errors.New("translate requires English text")
	}
	return utteranceInput{text: text.Text, prosody: text.Prosody, final: text.Final}, nil
}

func decodeTranslation(event qol.Event) (utteranceInput, error) {
	if event.Type != qol.TypeTranslationFinal {
		return utteranceInput{}, errors.New("tts requires final translation")
	}
	translation, err := wire.UnmarshalTranslation(event.Payload)
	if err != nil {
		return utteranceInput{}, err
	}
	if translation.TargetLanguage != "es" {
		return utteranceInput{}, errors.New("tts requires Spanish translation")
	}
	return utteranceInput{text: translation.Text, prosody: translation.Prosody, final: translation.Final}, nil
}

func singleLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

var _ qol.Stage = (*UtteranceStage)(nil)
