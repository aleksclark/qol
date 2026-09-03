package eventbus

import (
	"context"
	"errors"
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

func TestSubscribeHandlerFailureTerminatesDelivery(t *testing.T) {
	bus, cleanup := startTestBus(t)
	defer cleanup()
	ctx := context.Background()
	wantErr := errors.New("handler failed")
	deliveries := make(chan qol.Event, 2)
	sub, err := bus.Subscribe(ctx, "graph.err", qol.SubscribeOptions{}, func(_ context.Context, event qol.Event) error {
		deliveries <- event
		return wantErr
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	if err := bus.conn.Flush(); err != nil {
		t.Fatal(err)
	}

	publishEvent(t, bus, ctx, "graph.err", "evt-err")
	select {
	case <-deliveries:
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not called")
	}
	waitDone(t, sub)
	if got := sub.Err(); got != wantErr {
		t.Fatalf("subscription error = %v, want %v", got, wantErr)
	}

	publishEvent(t, bus, ctx, "graph.err", "evt-later")
	assertNoDelivery(t, deliveries)
}

func TestSubscribeDecodeFailureTerminatesDelivery(t *testing.T) {
	bus, cleanup := startTestBus(t)
	defer cleanup()
	ctx := context.Background()
	failedDeliveries := make(chan qol.Event, 1)
	unrelatedDeliveries := make(chan qol.Event, 1)
	sub, err := bus.Subscribe(ctx, "graph.malformed", qol.SubscribeOptions{}, func(_ context.Context, event qol.Event) error {
		failedDeliveries <- event
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	unrelated, err := bus.Subscribe(ctx, "graph.unrelated", qol.SubscribeOptions{}, func(_ context.Context, event qol.Event) error {
		unrelatedDeliveries <- event
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Close() })
	if err := bus.conn.Flush(); err != nil {
		t.Fatal(err)
	}

	malformed := []byte{0xff}
	wantErr := mustDecodeError(t, malformed)
	if err := bus.conn.Publish(subject("graph.malformed"), malformed); err != nil {
		t.Fatal(err)
	}
	if err := bus.conn.Flush(); err != nil {
		t.Fatal(err)
	}
	waitDone(t, sub)
	if got := sub.Err(); got == nil || got.Error() != wantErr.Error() {
		t.Fatalf("subscription error = %v, want %v", got, wantErr)
	}

	publishEvent(t, bus, ctx, "graph.malformed", "evt-later")
	assertNoDelivery(t, failedDeliveries)
	publishEvent(t, bus, ctx, "graph.unrelated", "evt-unrelated")
	select {
	case <-unrelatedDeliveries:
	case <-time.After(3 * time.Second):
		t.Fatal("unrelated subscription did not receive its event")
	}
}

func TestSubscribeHandlerFailureWinsOverConcurrentCancellation(t *testing.T) {
	bus, cleanup := startTestBus(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wantErr := errors.New("handler failed")
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	sub, err := bus.Subscribe(ctx, "graph.concurrent", qol.SubscribeOptions{}, func(context.Context, qol.Event) error {
		close(handlerStarted)
		<-releaseHandler
		return wantErr
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	if err := bus.conn.Flush(); err != nil {
		t.Fatal(err)
	}

	publishEvent(t, bus, ctx, "graph.concurrent", "evt-concurrent")
	select {
	case <-handlerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not called")
	}
	cancel()
	close(releaseHandler)
	waitDone(t, sub)
	if got := sub.Err(); got != wantErr {
		t.Fatalf("subscription error = %v, want %v", got, wantErr)
	}
}

func TestSubscribeCancellationAndCloseAreIdempotent(t *testing.T) {
	bus, cleanup := startTestBus(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(ctx, "graph.cancel", qol.SubscribeOptions{}, func(context.Context, qol.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	cancel()
	waitDone(t, sub)
	if got := sub.Err(); got != nil {
		t.Fatalf("subscription error = %v, want nil after cancellation", got)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	select {
	case <-sub.Done():
	default:
		t.Fatal("done channel was not left closed")
	}
}

func publishEvent(t *testing.T, bus *Bus, ctx context.Context, channel, id string) {
	t.Helper()
	if err := bus.Publish(ctx, qol.Event{ID: id, SessionID: "sess-" + id, Channel: channel, Type: qol.TypeAudioStream, Encoding: qol.EncodingProtobuf, Payload: []byte{9}}); err != nil {
		t.Fatal(err)
	}
	if err := bus.conn.Flush(); err != nil {
		t.Fatal(err)
	}
}

func mustDecodeError(t *testing.T, payload []byte) error {
	t.Helper()
	_, err := decode(payload)
	if err == nil {
		t.Fatal("malformed payload decoded successfully")
	}
	return err
}

func waitDone(t *testing.T, sub qol.Subscription) {
	t.Helper()
	select {
	case <-sub.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("subscription did not terminate")
	}
}

func assertNoDelivery(t *testing.T, deliveries <-chan qol.Event) {
	t.Helper()
	select {
	case event := <-deliveries:
		t.Fatalf("unexpected delivery after termination: %q", event.ID)
	case <-time.After(200 * time.Millisecond):
	}
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
