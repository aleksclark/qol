package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	qol "github.com/aleksclark/qol"
)

const publishChunkSize = 64 * 1024

type StreamProcessor interface {
	Process(context.Context, io.Reader, io.Writer) error
}

type liveProcessor interface {
	Start(context.Context) (io.WriteCloser, io.ReadCloser, func() error, error)
}

type StreamAudioStore interface {
	Open(context.Context, string) (StreamAudioWriter, error)
}

type StreamAudioWriter interface {
	Write([]byte) error
	Close() (qol.StoredAudio, error)
	Abort() error
}

type chunk struct {
	data []byte
	end  bool
}

type chunkDecoder func(qol.Event) (chunk, error)
type payloadEncoder func(chunk) ([]byte, error)

type streamState struct {
	path    string
	next    uint64
	outSeq  uint64
	parents []string
	span    qol.Span
	writer  StreamAudioWriter
	input   io.WriteCloser
	output  io.ReadCloser
	wait    func() error
	done    chan error
	cancel  context.CancelFunc
}

type Stage struct {
	spec           qol.StageSpec
	consumer       string
	group          string
	processor      StreamProcessor
	spoolDir       string
	decode         chunkDecoder
	encode         payloadEncoder
	outputType     string
	live           bool
	eager          bool
	now            func() time.Time
	processContext context.Context
	mu             sync.Mutex
	streams        map[string]*streamState
}

type Record struct {
	spec           qol.StageSpec
	group          string
	store          StreamAudioStore
	spoolDir       string
	now            func() time.Time
	processContext context.Context
	mu             sync.Mutex
	streams        map[string]*streamState
}

func newStage(name, consumer, group, inputChannel string, inputTypes []string, outputChannel, outputType string, processor StreamProcessor, spoolDir string, decode chunkDecoder, encode payloadEncoder) *Stage {
	return &Stage{
		spec:     qol.StageSpec{Name: name, Inputs: []qol.Port{{Name: "input", Channel: inputChannel, Types: inputTypes}}, Outputs: []qol.Port{{Name: "output", Channel: outputChannel, Types: []string{outputType}}}},
		consumer: consumer, group: group, processor: processor, spoolDir: spoolDir, decode: decode, encode: encode, outputType: outputType, now: time.Now, streams: make(map[string]*streamState),
	}
}

func (s *Stage) Spec() qol.StageSpec {
	return s.spec
}

func (s *Stage) Run(ctx context.Context, bus qol.Bus) error {
	s.processContext = ctx
	subscription, err := bus.Subscribe(ctx, s.spec.Inputs[0].Channel, durable(s.consumer, s.group), func(eventContext context.Context, event qol.Event) error {
		return s.handle(eventContext, bus, event)
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

func (s *Stage) handle(ctx context.Context, publisher qol.Publisher, event qol.Event) error {
	if s.live {
		return s.handleLive(ctx, publisher, event)
	}
	if s.eager {
		return s.handleEager(ctx, publisher, event)
	}
	value, err := s.decode(event)
	if err != nil {
		return err
	}
	inputPath, complete, parents, span, err := appendChunk(&s.mu, s.streams, s.spoolDir, s.spec.Name, event, value)
	if err != nil || !complete {
		return err
	}
	defer os.Remove(inputPath)
	input, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("open stage input: %w", err)
	}
	defer input.Close()
	output, err := os.CreateTemp(s.spoolDir, s.spec.Name+"-output-*")
	if err != nil {
		return fmt.Errorf("create stage output: %w", err)
	}
	outputPath := output.Name()
	defer os.Remove(outputPath)
	if err := s.processor.Process(ctx, input, output); err != nil {
		output.Close()
		return fmt.Errorf("%s stream: %w", s.spec.Name, err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close stage output: %w", err)
	}
	return s.publish(ctx, publisher, event.SessionID, parents, span, outputPath)
}

func (s *Stage) handleLive(ctx context.Context, publisher qol.Publisher, event qol.Event) error {
	value, err := s.decode(event)
	if err != nil {
		return err
	}
	state, skipWrite, err := s.liveStream(ctx, publisher, event)
	if err != nil {
		return err
	}
	if !skipWrite && len(value.data) > 0 {
		if _, err := state.input.Write(value.data); err != nil {
			s.abortLive(event)
			return fmt.Errorf("write live %s input: %w", s.spec.Name, err)
		}
	}
	if !value.end {
		return nil
	}
	if err := state.input.Close(); err != nil {
		s.abortLive(event)
		return fmt.Errorf("close live %s input: %w", s.spec.Name, err)
	}
	if err := <-state.done; err != nil {
		s.abortLive(event)
		return err
	}
	s.dropLive(event)
	return nil
}

func (s *Stage) handleEager(ctx context.Context, publisher qol.Publisher, event qol.Event) error {
	value, err := s.decode(event)
	if err != nil {
		return err
	}
	state, skip, err := s.eagerStream(event)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
	if !value.end {
		if isDummyEOS(value.data) {
			return nil
		}
		return s.processEager(ctx, publisher, event.SessionID, state, value.data)
	}
	if !isDummyEOS(value.data) && len(bytes.TrimSpace(value.data)) > 0 {
		if err := s.processEager(ctx, publisher, event.SessionID, state, value.data); err != nil {
			return err
		}
	}
	if err := s.publishEOS(ctx, publisher, event.SessionID, state); err != nil {
		return err
	}
	s.dropEager(event)
	return nil
}

func (s *Stage) eagerStream(event qol.Event) (*streamState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := event.SessionID + "\x00" + event.Channel
	state := s.streams[key]
	if state != nil {
		if event.Seq < state.next {
			return state, true, nil
		}
		if event.Seq != state.next {
			return nil, false, fmt.Errorf("%s expected sequence %d, got %d", s.spec.Name, state.next, event.Seq)
		}
		state.next++
		state.parents = append(state.parents, event.ID)
		if event.Span.End > state.span.End {
			state.span.End = event.Span.End
		}
		return state, false, nil
	}
	if event.Seq != 0 {
		return nil, false, fmt.Errorf("%s stream starts at sequence %d", s.spec.Name, event.Seq)
	}
	state = &streamState{next: 1, parents: []string{event.ID}, span: event.Span}
	s.streams[key] = state
	return state, false, nil
}

func (s *Stage) processEager(ctx context.Context, publisher qol.Publisher, sessionID string, state *streamState, data []byte) error {
	if err := os.MkdirAll(s.spoolDir, 0o750); err != nil {
		return err
	}
	output, err := os.CreateTemp(s.spoolDir, s.spec.Name+"-output-*")
	if err != nil {
		return fmt.Errorf("create stage output: %w", err)
	}
	outputPath := output.Name()
	defer os.Remove(outputPath)
	if err := s.processor.Process(ctx, bytes.NewReader(data), output); err != nil {
		output.Close()
		return fmt.Errorf("%s stream: %w", s.spec.Name, err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close stage output: %w", err)
	}
	return s.publishChunks(ctx, publisher, sessionID, state, outputPath, false)
}

func (s *Stage) publishEOS(ctx context.Context, publisher qol.Publisher, sessionID string, state *streamState) error {
	if s.spec.Name == "tts" {
		return s.publishBytes(ctx, publisher, sessionID, state, []byte{0, 0}, true)
	}
	return s.publishText(ctx, publisher, sessionID, state, "", true)
}

func (s *Stage) dropEager(event qol.Event) {
	s.mu.Lock()
	delete(s.streams, event.SessionID+"\x00"+event.Channel)
	s.mu.Unlock()
}

func isDummyEOS(data []byte) bool {
	return len(bytes.TrimSpace(data)) == 0
}

func (s *Stage) liveStream(ctx context.Context, publisher qol.Publisher, event qol.Event) (*streamState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := event.SessionID + "\x00" + event.Channel
	state := s.streams[key]
	if state != nil {
		if event.Seq < state.next {
			return state, true, nil
		}
		if event.Seq != state.next {
			return nil, false, fmt.Errorf("%s expected sequence %d, got %d", s.spec.Name, state.next, event.Seq)
		}
		state.next++
		state.parents = append(state.parents, event.ID)
		if event.Span.End > state.span.End {
			state.span.End = event.Span.End
		}
		return state, false, nil
	}
	if event.Seq != 0 {
		return nil, false, fmt.Errorf("%s stream starts at sequence %d", s.spec.Name, event.Seq)
	}
	starter, ok := s.processor.(liveProcessor)
	if !ok {
		return nil, false, fmt.Errorf("%s requires a live processor", s.spec.Name)
	}
	processCtx := s.processContext
	if processCtx == nil {
		processCtx = ctx
	}
	processCtx, cancel := context.WithCancel(processCtx)
	input, output, wait, err := starter.Start(processCtx)
	if err != nil {
		cancel()
		return nil, false, err
	}
	state = &streamState{
		next:    1,
		parents: []string{event.ID},
		span:    event.Span,
		input:   input,
		output:  output,
		wait:    wait,
		done:    make(chan error, 1),
		cancel:  cancel,
	}
	s.streams[key] = state
	go s.consumeLive(processCtx, publisher, event.SessionID, state)
	return state, false, nil
}

func (s *Stage) consumeLive(ctx context.Context, publisher qol.Publisher, sessionID string, state *streamState) {
	defer state.output.Close()
	reader := bufio.NewReader(state.output)
	var processErr error
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			text := strings.TrimSpace(line)
			if text != "" {
				if pubErr := s.publishText(ctx, publisher, sessionID, state, text, false); pubErr != nil {
					processErr = pubErr
					break
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				processErr = err
			}
			break
		}
	}
	if waitErr := state.wait(); waitErr != nil && processErr == nil {
		processErr = fmt.Errorf("%s stream: %w", s.spec.Name, waitErr)
	}
	if processErr == nil {
		processErr = s.publishText(ctx, publisher, sessionID, state, "", true)
	}
	state.done <- processErr
}

func (s *Stage) publishText(ctx context.Context, publisher qol.Publisher, sessionID string, state *streamState, text string, end bool) error {
	if text == "" && !end {
		return nil
	}
	if text == "" && end {
		text = " "
	}
	payload, err := s.encode(chunk{data: []byte(text), end: end})
	if err != nil {
		return err
	}
	s.mu.Lock()
	sequence := state.outSeq
	state.outSeq++
	parents := append([]string(nil), state.parents...)
	span := state.span
	s.mu.Unlock()
	return publisher.Publish(ctx, qol.Event{
		ID:         eventID(s.spec.Name, sessionID, sequence),
		SessionID:  sessionID,
		Channel:    s.spec.Outputs[0].Channel,
		Type:       s.outputType,
		Seq:        sequence,
		Span:       span,
		Parents:    parents,
		ProducedAt: s.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	})
}

func (s *Stage) abortLive(event qol.Event) {
	s.mu.Lock()
	state := s.streams[event.SessionID+"\x00"+event.Channel]
	delete(s.streams, event.SessionID+"\x00"+event.Channel)
	s.mu.Unlock()
	if state == nil {
		return
	}
	if state.cancel != nil {
		state.cancel()
	}
	if state.input != nil {
		_ = state.input.Close()
	}
}

func (s *Stage) dropLive(event qol.Event) {
	s.mu.Lock()
	state := s.streams[event.SessionID+"\x00"+event.Channel]
	delete(s.streams, event.SessionID+"\x00"+event.Channel)
	s.mu.Unlock()
	if state != nil && state.cancel != nil {
		state.cancel()
	}
}

func (s *Stage) publish(ctx context.Context, publisher qol.Publisher, sessionID string, parents []string, span qol.Span, path string) error {
	state := &streamState{parents: parents, span: span}
	return s.publishChunks(ctx, publisher, sessionID, state, path, true)
}

func (s *Stage) publishChunks(ctx context.Context, publisher qol.Publisher, sessionID string, state *streamState, path string, end bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	buffer := make([]byte, publishChunkSize)
	var wrote bool
	for {
		size, readErr := io.ReadFull(file, buffer)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return readErr
		}
		last := errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF)
		if size > 0 {
			if err := s.publishBytes(ctx, publisher, sessionID, state, append([]byte(nil), buffer[:size]...), end && last); err != nil {
				return err
			}
			wrote = true
		}
		if last {
			if end && !wrote {
				return s.publishEOS(ctx, publisher, sessionID, state)
			}
			return nil
		}
	}
}

func (s *Stage) publishBytes(ctx context.Context, publisher qol.Publisher, sessionID string, state *streamState, data []byte, end bool) error {
	payload, err := s.encode(chunk{data: data, end: end})
	if err != nil {
		return err
	}
	s.mu.Lock()
	sequence := state.outSeq
	state.outSeq++
	parents := append([]string(nil), state.parents...)
	span := state.span
	s.mu.Unlock()
	eventType := s.outputType
	if s.spec.Name == "tts" && end {
		eventType = qol.TypeAudioPCMFinal
	}
	return publisher.Publish(ctx, qol.Event{
		ID:         eventID(s.spec.Name, sessionID, sequence),
		SessionID:  sessionID,
		Channel:    s.spec.Outputs[0].Channel,
		Type:       eventType,
		Seq:        sequence,
		Span:       span,
		Parents:    parents,
		ProducedAt: s.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	})
}

func appendChunk(mu *sync.Mutex, streams map[string]*streamState, spoolDir, name string, event qol.Event, value chunk) (string, bool, []string, qol.Span, error) {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(spoolDir, 0o750); err != nil {
		return "", false, nil, qol.Span{}, err
	}
	key := event.SessionID + "\x00" + event.Channel
	state := streams[key]
	if state == nil {
		if event.Seq != 0 {
			return "", false, nil, qol.Span{}, fmt.Errorf("%s stream starts at sequence %d", name, event.Seq)
		}
		file, err := os.CreateTemp(spoolDir, name+"-input-*")
		if err != nil {
			return "", false, nil, qol.Span{}, err
		}
		if err := file.Close(); err != nil {
			return "", false, nil, qol.Span{}, err
		}
		state = &streamState{path: file.Name(), span: event.Span}
		streams[key] = state
	}
	if event.Seq < state.next {
		return state.path, false, nil, state.span, nil
	}
	if event.Seq != state.next {
		return "", false, nil, qol.Span{}, fmt.Errorf("%s expected sequence %d, got %d", name, state.next, event.Seq)
	}
	file, err := os.OpenFile(state.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return "", false, nil, qol.Span{}, err
	}
	_, writeErr := file.Write(value.data)
	closeErr := file.Close()
	if writeErr != nil {
		return "", false, nil, qol.Span{}, writeErr
	}
	if closeErr != nil {
		return "", false, nil, qol.Span{}, closeErr
	}
	state.next++
	state.parents = append(state.parents, event.ID)
	if event.Span.End > state.span.End {
		state.span.End = event.Span.End
	}
	if !value.end {
		return state.path, false, nil, state.span, nil
	}
	delete(streams, key)
	return state.path, true, append([]string(nil), state.parents...), state.span, nil
}

func durable(consumer, group string) qol.SubscribeOptions {
	return qol.SubscribeOptions{Group: group}
}

func SafeSessionName(sessionID string) string {
	return filepath.Base(sessionID)
}

func eventID(stage, sessionID string, sequence uint64) string {
	hash := sha256.New()
	for _, part := range []string{stage, sessionID, strconv.FormatUint(sequence, 10)} {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

var _ qol.Stage = (*Stage)(nil)
