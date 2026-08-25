package eventbus

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	qol "github.com/aleksclark/qol"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestSubscribeFanOutDoesNotSteal(t *testing.T) {
	bus, cleanup := startTestBus(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var mu sync.Mutex
	fanout := make([]qol.Event, 0, 1)
	queued := make([]qol.Event, 0, 1)
	fanSub, err := bus.Subscribe(ctx, "graph.source", qol.SubscribeOptions{}, func(_ context.Context, event qol.Event) error {
		mu.Lock()
		fanout = append(fanout, event)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fanSub.Close() })
	queueSub, err := bus.Subscribe(ctx, "graph.source", qol.SubscribeOptions{Group: "record"}, func(_ context.Context, event qol.Event) error {
		mu.Lock()
		queued = append(queued, event)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queueSub.Close() })

	event := qol.Event{
		ID:         "evt-1",
		SessionID:  "sess-1",
		Channel:    "graph.source",
		Type:       qol.TypeAudioStream,
		Encoding:   qol.EncodingProtobuf,
		Payload:    []byte{1, 2, 3},
		ProducedAt: time.Now().UTC(),
	}
	if err := bus.Publish(ctx, event); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := len(fanout) == 1 && len(queued) == 1
		mu.Unlock()
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("fan-out=%d queued=%d, want both 1", len(fanout), len(queued))
}

func TestSubscribeRecordsHandlerError(t *testing.T) {
	bus, cleanup := startTestBus(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub, err := bus.Subscribe(ctx, "graph.err", qol.SubscribeOptions{}, func(context.Context, qol.Event) error {
		return context.DeadlineExceeded
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	if err := bus.Publish(ctx, qol.Event{
		ID:        "evt-err",
		SessionID: "sess-err",
		Channel:   "graph.err",
		Type:      qol.TypeAudioStream,
		Encoding:  qol.EncodingProtobuf,
		Payload:   []byte{9},
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sub.Err() != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("handler error was not recorded")
}

func startTestBus(t *testing.T) (*Bus, func()) {
	t.Helper()
	if url := os.Getenv("NATS_URL"); url != "" {
		conn, err := nats.Connect(url)
		if err != nil {
			t.Fatalf("NATS_URL %s: %v", url, err)
		}
		bus, err := New(conn)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		return bus, conn.Close
	}
	opts := &natsserver.Options{Port: -1, NoLog: true, NoSigs: true}
	server, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go server.Start()
	if !server.ReadyForConnections(5 * time.Second) {
		t.Fatal("in-process NATS did not start")
	}
	conn, err := nats.Connect(server.ClientURL())
	if err != nil {
		server.Shutdown()
		t.Fatal(err)
	}
	bus, err := New(conn)
	if err != nil {
		conn.Close()
		server.Shutdown()
		t.Fatal(err)
	}
	return bus, func() {
		conn.Close()
		server.Shutdown()
	}
}
