package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

type STT struct {
	spec           qol.StageSpec
	group          string
	processor      LiveProcessor
	now            func() time.Time
	processContext context.Context
	mu             sync.Mutex
	streams        map[string]*streamState
}

func NewSTT(processor LiveProcessor) *STT {
	return &STT{
		spec: qol.StageSpec{
			Name:    "stt-whisper",
			Inputs:  []qol.Port{{Name: "input", Channel: qol.ChannelAudioInput, Types: []string{qol.TypeAudioStream}}},
			Outputs: []qol.Port{{Name: "output", Channel: qol.ChannelUtterances, Types: []string{qol.TypeTextFinal}}},
		},
		group: "qol-stt-whisper", processor: processor, now: time.Now, streams: make(map[string]*streamState),
	}
}

func (s *STT) Spec() qol.StageSpec {
	return s.spec
}

func (s *STT) Run(ctx context.Context, bus qol.Bus) error {
	s.processContext = ctx
	return runSubscription(ctx, bus, s.spec.Inputs[0].Channel, s.group, func(eventContext context.Context, event qol.Event) error {
		return s.handle(eventContext, bus, event)
	})
}

func (s *STT) handle(ctx context.Context, publisher qol.Publisher, event qol.Event) error {
	value, err := decodeAudioStream(event)
	if err != nil {
		return err
	}
	state, skipWrite, err := s.liveStream(ctx, publisher, event)
	if err != nil {
		return err
	}
	if !skipWrite && len(value.data) > 0 {
		if _, err := state.input.Write(value.data); err != nil {
			s.abort(event)
			return fmt.Errorf("write live %s input: %w", s.spec.Name, err)
		}
	}
	if !value.end {
		return nil
	}
	if err := state.input.Close(); err != nil {
		s.abort(event)
		return fmt.Errorf("close live %s input: %w", s.spec.Name, err)
	}
	if err := <-state.done; err != nil {
		s.abort(event)
		return err
	}
	s.drop(event)
	return nil
}

func (s *STT) liveStream(ctx context.Context, publisher qol.Publisher, event qol.Event) (*streamState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := streamKey(event)
	state := s.streams[key]
	if state != nil {
		if event.Seq < state.next {
			return state, true, nil
		}
		if event.Seq != state.next {
			return nil, false, fmt.Errorf("%s expected sequence %d, got %d", s.spec.Name, state.next, event.Seq)
		}
		state.next++
		return state, false, nil
	}
	if event.Seq != 0 {
		return nil, false, fmt.Errorf("%s stream starts at sequence %d", s.spec.Name, event.Seq)
	}
	processCtx := s.processContext
	if processCtx == nil {
		processCtx = ctx
	}
	processCtx, cancel := context.WithCancel(processCtx)
	input, output, wait, err := s.processor.Start(processCtx)
	if err != nil {
		cancel()
		return nil, false, err
	}
	state = &streamState{next: 1, parents: []string{event.ID}, input: input, output: output, wait: wait, done: make(chan error, 1), cancel: cancel}
	s.streams[key] = state
	go s.consume(processCtx, publisher, event.SessionID, state)
	return state, false, nil
}

func (s *STT) consume(ctx context.Context, publisher qol.Publisher, sessionID string, state *streamState) {
	defer state.output.Close()
	reader := bufio.NewReader(state.output)
	var processErr error
	var last qol.Span
	for {
		utterance, err := wire.ReadUtterance(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				processErr = fmt.Errorf("%s output: %w", s.spec.Name, err)
			}
			break
		}
		last = utterance.Span
		if err := s.publishText(ctx, publisher, sessionID, state, utterance.Span, utterance.Text, false); err != nil {
			processErr = err
			break
		}
	}
	if processErr != nil {
		state.cancel()
	}
	if waitErr := state.wait(); waitErr != nil && processErr == nil {
		processErr = fmt.Errorf("%s stream: %w", s.spec.Name, waitErr)
	}
	if processErr == nil {
		processErr = s.publishText(ctx, publisher, sessionID, state, qol.Span{Start: last.End, End: last.End}, " ", true)
	}
	state.done <- processErr
}

func (s *STT) publishText(ctx context.Context, publisher qol.Publisher, sessionID string, state *streamState, span qol.Span, text string, final bool) error {
	payload, err := wire.MarshalText(qol.Text{Text: text, Final: final, Language: "en"})
	if err != nil {
		return err
	}
	s.mu.Lock()
	sequence := state.outSeq
	state.outSeq++
	parents := append([]string(nil), state.parents...)
	s.mu.Unlock()
	return publisher.Publish(ctx, qol.Event{
		ID:         eventID(s.spec.Name, sessionID, sequence),
		SessionID:  sessionID,
		Channel:    s.spec.Outputs[0].Channel,
		Type:       qol.TypeTextFinal,
		Seq:        sequence,
		Span:       span,
		Parents:    parents,
		ProducedAt: s.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	})
}

func (s *STT) abort(event qol.Event) {
	s.mu.Lock()
	state := s.streams[streamKey(event)]
	delete(s.streams, streamKey(event))
	s.mu.Unlock()
	if state == nil {
		return
	}
	state.cancel()
	_ = state.input.Close()
}

func (s *STT) drop(event qol.Event) {
	s.mu.Lock()
	state := s.streams[streamKey(event)]
	delete(s.streams, streamKey(event))
	s.mu.Unlock()
	if state != nil {
		state.cancel()
	}
}

var _ qol.Stage = (*STT)(nil)
