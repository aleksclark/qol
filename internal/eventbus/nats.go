package eventbus

import (
	"context"
	"fmt"
	"time"

	"github.com/aleksclark/qol/domain"
	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"github.com/aleksclark/qol/internal/wire"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

const StreamName = "QOL_EVENTS"

type Bus struct {
	jetstream nats.JetStreamContext
}

func New(connection *nats.Conn) (*Bus, error) {
	jetstream, err := connection.JetStream()
	if err != nil {
		return nil, err
	}
	if _, err := jetstream.StreamInfo(StreamName); err == nats.ErrStreamNotFound {
		_, err = jetstream.AddStream(&nats.StreamConfig{Name: StreamName, Subjects: []string{"qol.session.*.pcm.*"}, Storage: nats.FileStorage, MaxAge: 24 * time.Hour, Duplicates: 2 * time.Minute})
		if err != nil {
			return nil, fmt.Errorf("create event stream: %w", err)
		}
	}
	return &Bus{jetstream: jetstream}, nil
}

func (b *Bus) Publish(ctx context.Context, subject string, event domain.Event) error {
	message, err := wire.EventToProto(event)
	if err != nil {
		return err
	}
	payload, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	publication := nats.NewMsg(subject)
	publication.Data = payload
	publication.Header.Set(nats.MsgIdHdr, event.ID)
	_, err = b.jetstream.PublishMsg(publication, nats.Context(ctx))
	return err
}

func (b *Bus) SubscribeOutput(subject string, handler func(domain.Event) error) (*nats.Subscription, error) {
	return b.jetstream.Subscribe(subject, func(message *nats.Msg) {
		event, err := decode(message.Data)
		if err == nil {
			err = handler(event)
		}
		if err == nil {
			_ = message.Ack()
		} else {
			_ = message.Nak()
		}
	}, nats.ManualAck(), nats.AckExplicit(), nats.DeliverNew())
}

func (b *Bus) SubscribeInputs(durable string, handler func(context.Context, domain.Event) error) (*nats.Subscription, error) {
	return b.jetstream.QueueSubscribe("qol.session.*.pcm.input", "qol-echo", func(message *nats.Msg) {
		event, err := decode(message.Data)
		if err == nil {
			err = handler(context.Background(), event)
		}
		if err == nil {
			_ = message.Ack()
		} else {
			_ = message.Nak()
		}
	}, nats.Durable(durable), nats.ManualAck(), nats.AckExplicit(), nats.DeliverAll())
}

func decode(payload []byte) (domain.Event, error) {
	message := new(qolv1.Event)
	if err := proto.Unmarshal(payload, message); err != nil {
		return domain.Event{}, err
	}
	return wire.EventFromProto(message)
}
