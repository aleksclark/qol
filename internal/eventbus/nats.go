package eventbus

import (
	"context"
	"errors"
	"fmt"
	"sync"

	qol "github.com/aleksclark/qol"
	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"github.com/aleksclark/qol/internal/wire"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

const subjectPrefix = "qol."

type Bus struct {
	conn   *nats.Conn
	mu     sync.Mutex
	counts map[string]uint64
}

type TopicActivity struct {
	Subject    string
	EventCount uint64
}

type subscription struct {
	nats *nats.Subscription
	done chan struct{}
	once sync.Once

	mu       sync.Mutex
	err      error
	closeErr error

	lifecycleMu sync.Mutex
	terminated  bool
	callbacks   int
	doneClosed  bool
}

func New(connection *nats.Conn) (*Bus, error) {
	if connection == nil {
		return nil, errors.New("NATS connection is required")
	}
	return &Bus{conn: connection, counts: make(map[string]uint64)}, nil
}

func (b *Bus) WatchActivity() error {
	_, err := b.conn.Subscribe(subjectPrefix+"graph.>", func(message *nats.Msg) {
		b.mu.Lock()
		b.counts[message.Subject]++
		b.mu.Unlock()
	})
	if err != nil {
		return fmt.Errorf("watch graph activity: %w", err)
	}
	return nil
}

func (b *Bus) TopicActivity() ([]TopicActivity, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	activity := make([]TopicActivity, 0, len(b.counts))
	for topic, count := range b.counts {
		activity = append(activity, TopicActivity{Subject: topic, EventCount: count})
	}
	return activity, nil
}

func (b *Bus) Publish(ctx context.Context, event qol.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	message, err := wire.EventToProto(event)
	if err != nil {
		return err
	}
	payload, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return b.conn.Publish(subject(event.Channel), payload)
}

func (b *Bus) Subscribe(ctx context.Context, channel string, opts qol.SubscribeOptions, handler qol.Handler) (qol.Subscription, error) {
	if channel == "" {
		return nil, errors.New("subscription channel is required")
	}
	result := &subscription{done: make(chan struct{})}
	callback := func(message *nats.Msg) {
		if !result.beginCallback() {
			return
		}
		defer result.endCallback()

		event, err := decode(message.Data)
		if err != nil {
			result.terminate(err)
			return
		}
		if err := handler(ctx, event); err != nil {
			result.terminate(err)
		}
	}
	var err error
	if opts.Group == "" {
		result.nats, err = b.conn.Subscribe(subject(channel), callback)
	} else {
		result.nats, err = b.conn.QueueSubscribe(subject(channel), opts.Group, callback)
	}
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		result.terminate(ctx.Err())
	}()
	return result, nil
}

func (s *subscription) Close() error {
	s.terminate(nil)

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

func (s *subscription) Done() <-chan struct{} {
	return s.done
}

func (s *subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *subscription) terminate(terminalErr error) {
	if terminalErr != nil && !errors.Is(terminalErr, context.Canceled) {
		s.mu.Lock()
		if s.err == nil {
			s.err = terminalErr
		}
		s.mu.Unlock()
	}

	s.lifecycleMu.Lock()
	s.terminated = true
	s.lifecycleMu.Unlock()

	s.once.Do(func() {
		var closeErr error
		if s.nats != nil {
			closeErr = s.nats.Unsubscribe()
		}

		s.mu.Lock()
		s.closeErr = closeErr
		s.mu.Unlock()
		s.closeDoneWhenCallbacksFinish()
	})
}

func (s *subscription) beginCallback() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.terminated {
		return false
	}
	s.callbacks++
	return true
}

func (s *subscription) endCallback() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.callbacks--
	if s.terminated && s.callbacks == 0 && !s.doneClosed {
		s.doneClosed = true
		close(s.done)
	}
}

func (s *subscription) closeDoneWhenCallbacksFinish() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.callbacks == 0 && !s.doneClosed {
		s.doneClosed = true
		close(s.done)
	}
}

func subject(channel string) string {
	return subjectPrefix + channel
}

func decode(payload []byte) (qol.Event, error) {
	message := new(qolv1.Event)
	if err := proto.Unmarshal(payload, message); err != nil {
		return qol.Event{}, err
	}
	return wire.EventFromProto(message)
}

var _ qol.Bus = (*Bus)(nil)
