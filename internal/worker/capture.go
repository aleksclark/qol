package worker

import (
	"context"
	"errors"

	qol "github.com/aleksclark/qol"
)

type Capture struct {
	spec  qol.StageSpec
	group string
}

func NewCapture() *Capture {
	return &Capture{
		spec: qol.StageSpec{
			Name:    "capture",
			Inputs:  []qol.Port{{Name: "input", Channel: qol.ChannelCaptureInput, Types: []string{qol.TypeAudioStream}}},
			Outputs: []qol.Port{{Name: "output", Channel: qol.ChannelCaptureOutput, Types: []string{qol.TypeAudioStream}}, {Name: "stt", Channel: qol.ChannelAudioInput, Types: []string{qol.TypeAudioStream}}, {Name: "record-en", Channel: qol.ChannelRecordENInput, Types: []string{qol.TypeAudioStream}}},
		},
		group: "qol-capture",
	}
}

func (c *Capture) Spec() qol.StageSpec {
	return c.spec
}

func (c *Capture) Run(ctx context.Context, bus qol.Bus) error {
	subscription, err := bus.Subscribe(ctx, c.spec.Inputs[0].Channel, qol.SubscribeOptions{Group: c.group}, func(eventContext context.Context, event qol.Event) error {
		return c.handle(eventContext, bus, event)
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

func (c *Capture) handle(ctx context.Context, publisher qol.Publisher, event qol.Event) error {
	if event.Type != qol.TypeAudioStream {
		return errors.New("capture requires an audio stream event")
	}
	for _, port := range c.spec.Outputs {
		out := event
		out.ID = eventID(c.spec.Name+"."+port.Name, event.SessionID, event.Seq)
		out.Channel = port.Channel
		out.Parents = []string{event.ID}
		if err := publisher.Publish(ctx, out); err != nil {
			return err
		}
	}
	return nil
}

var _ qol.Stage = (*Capture)(nil)
