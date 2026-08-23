package worker_test

import (
	"context"
	"testing"

	"github.com/aleksclark/qol/domain"
	"github.com/aleksclark/qol/internal/worker"
)

type publisher struct {
	subject string
	event   domain.Event
}

func (p *publisher) Publish(_ context.Context, subject string, event domain.Event) error {
	p.subject, p.event = subject, event
	return nil
}

func TestEchoPreservesPCMAndLinksParent(t *testing.T) {
	output := new(publisher)
	echo := worker.NewEcho(output)
	input := domain.Event{ID: "input", SessionID: "session", Sequence: 2, PCM: domain.PCM{SampleRate: 16000, Channels: 1, Frames: 1, Data: []byte{1, 2}}}
	if err := echo.Handle(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if output.subject != "qol.session.session.pcm.output" || output.event.ID == input.ID || output.event.Parents[0] != input.ID || string(output.event.PCM.Data) != string(input.PCM.Data) {
		t.Fatalf("unexpected output: %#v", output)
	}
}
