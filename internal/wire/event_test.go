package wire_test

import (
	"testing"
	"time"

	"github.com/aleksclark/qol/domain"
	"github.com/aleksclark/qol/internal/wire"
)

func TestEventRoundTrip(t *testing.T) {
	input := domain.Event{ID: "event", SessionID: "session", Sequence: 7, Span: domain.Span{End: 20 * domain.MediaTime(time.Millisecond)}, Produced: time.Unix(10, 0), PCM: domain.PCM{SampleRate: 16000, Channels: 1, Frames: 2, Data: []byte{1, 2, 3, 4}}}
	message, err := wire.EventToProto(input)
	if err != nil {
		t.Fatal(err)
	}
	output, err := wire.EventFromProto(message)
	if err != nil {
		t.Fatal(err)
	}
	if output.ID != input.ID || output.Sequence != input.Sequence || string(output.PCM.Data) != string(input.PCM.Data) {
		t.Fatalf("unexpected round trip: %#v", output)
	}
}

func TestRejectsInvalidPCM(t *testing.T) {
	_, err := wire.EventToProto(domain.Event{ID: "event", SessionID: "session", PCM: domain.PCM{SampleRate: 16000, Channels: 1, Frames: 2, Data: []byte{1}}})
	if err == nil {
		t.Fatal("expected invalid PCM to fail")
	}
}
