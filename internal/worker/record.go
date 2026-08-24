package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

func NewRecord(name, input, output string, store StreamAudioStore, spoolDir string) *Record {
	return &Record{
		spec:     qol.StageSpec{Name: name, Inputs: []qol.Port{{Name: "audio", Channel: input, Types: []string{qol.TypeAudioStream, qol.TypeAudioPCM, qol.TypeAudioPCMFinal}}}, Outputs: []qol.Port{{Name: "completed", Channel: output, Types: []string{qol.TypeStoredAudio}}}},
		group:    "qol-" + name, store: store, spoolDir: spoolDir, now: time.Now, streams: make(map[string]*streamState),
	}
}

func (r *Record) Spec() qol.StageSpec {
	return r.spec
}

func (r *Record) Run(ctx context.Context, bus qol.Bus) error {
	r.processContext = ctx
	subscription, err := bus.Subscribe(ctx, r.spec.Inputs[0].Channel, qol.SubscribeOptions{Group: r.group}, func(eventContext context.Context, event qol.Event) error {
		return r.handle(eventContext, bus, event)
	})
	if err != nil {
		return err
	}
	defer subscription.Close()
	select {
	case <-ctx.Done():
		return nil
	case <-subscription.Done():
		return subscription.Err()
	}
}

func (r *Record) handle(ctx context.Context, publisher qol.Publisher, event qol.Event) error {
	value, err := decodeRecordAudio(event)
	if err != nil {
		return err
	}
	writer, created, err := r.streamWriter(ctx, event)
	if err != nil {
		return err
	}
	if err := writer.Write(value.data); err != nil {
		_ = writer.Abort()
		r.dropStream(event)
		return fmt.Errorf("write Ogg/Opus stream: %w", err)
	}
	if !value.end {
		return nil
	}
	stored, err := writer.Close()
	r.dropStream(event)
	if err != nil {
		return fmt.Errorf("store Ogg/Opus: %w", err)
	}
	payload, err := wire.MarshalStoredAudio(stored)
	if err != nil {
		return err
	}
	completed := qol.Event{ID: eventID(r.spec.Name, event.SessionID, 0), SessionID: event.SessionID, Channel: r.spec.Outputs[0].Channel, Type: qol.TypeStoredAudio, Span: created.span, Parents: append([]string(nil), created.parents...), ProducedAt: r.now().UTC(), Encoding: qol.EncodingProtobuf, Payload: payload}
	return publisher.Publish(ctx, completed)
}

func (r *Record) streamWriter(ctx context.Context, event qol.Event) (StreamAudioWriter, *streamState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := event.SessionID + "\x00" + event.Channel
	state := r.streams[key]
	if state == nil {
		if event.Seq != 0 {
			return nil, nil, fmt.Errorf("%s stream starts at sequence %d", r.spec.Name, event.Seq)
		}
		processContext := r.processContext
		if processContext == nil {
			processContext = ctx
		}
		writer, err := r.store.Open(processContext, event.SessionID+"-"+r.spec.Name)
		if err != nil {
			return nil, nil, err
		}
		state = &streamState{writer: writer, span: event.Span}
		r.streams[key] = state
	}
	if event.Seq < state.next {
		return state.writer, state, nil
	}
	if event.Seq != state.next {
		return nil, nil, fmt.Errorf("%s expected sequence %d, got %d", r.spec.Name, state.next, event.Seq)
	}
	state.next++
	state.parents = append(state.parents, event.ID)
	if event.Span.End > state.span.End {
		state.span.End = event.Span.End
	}
	return state.writer, state, nil
}

func (r *Record) dropStream(event qol.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.streams, event.SessionID+"\x00"+event.Channel)
}

func decodeRecordAudio(event qol.Event) (chunk, error) {
	switch event.Type {
	case qol.TypeAudioStream:
		return decodeAudioStream(event)
	case qol.TypeAudioPCM, qol.TypeAudioPCMFinal:
		return decodeAudio(event)
	default:
		return chunk{}, errors.New("record requires audio")
	}
}

func decodeAudio(event qol.Event) (chunk, error) {
	if event.Type != qol.TypeAudioPCM && event.Type != qol.TypeAudioPCMFinal {
		return chunk{}, errors.New("record requires PCM audio")
	}
	audio, err := wire.UnmarshalAudio(event.Payload)
	if err != nil {
		return chunk{}, err
	}
	if audio.Format.Format != qol.SampleS16LE {
		return chunk{}, errors.New("record requires S16LE audio")
	}
	return chunk{data: audio.Data, end: event.Type == qol.TypeAudioPCMFinal}, nil
}

var _ qol.Stage = (*Record)(nil)
