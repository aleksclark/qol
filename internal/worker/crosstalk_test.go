package worker_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/cmplx"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/aleksclark/crosstalk/abc"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/media"
	"github.com/aleksclark/qol/internal/wire"
	"github.com/aleksclark/qol/internal/worker"
)

func TestCrosstalkSpecRejectsSelfLoopAndEmpty(t *testing.T) {
	if _, err := worker.NewCrosstalk(worker.CrosstalkConfig{}); err == nil {
		t.Fatal("expected empty config to fail")
	}
	if _, err := worker.NewCrosstalk(worker.CrosstalkConfig{InputChannel: "same", OutputChannel: "same"}); err == nil {
		t.Fatal("expected self-loop to fail")
	}
	stage, err := worker.NewCrosstalk(worker.CrosstalkConfig{
		Name:          "bridge",
		OutputChannel: "graph.crosstalk.out",
		OutputProfile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: 16000, Channels: 1},
		Dial:          func(context.Context, worker.CrosstalkConfig) (worker.ABCSession, error) { return nil, errors.New("unused") },
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := stage.Spec()
	if spec.Name != "bridge" || len(spec.Inputs) != 0 || len(spec.Outputs) != 1 || spec.Outputs[0].Channel != "graph.crosstalk.out" {
		t.Fatalf("source-only spec: %#v", spec)
	}
	stage, err = worker.NewCrosstalk(worker.CrosstalkConfig{
		InputChannel: "graph.tts.audio",
		Dial:         func(context.Context, worker.CrosstalkConfig) (worker.ABCSession, error) { return nil, errors.New("unused") },
	})
	if err != nil {
		t.Fatal(err)
	}
	spec = stage.Spec()
	if len(spec.Outputs) != 0 || len(spec.Inputs) != 1 || spec.Inputs[0].Channel != "graph.tts.audio" {
		t.Fatalf("sink-only spec: %#v", spec)
	}
}

func TestCrosstalkSourcesPCM(t *testing.T) {
	requireFFmpeg(t)
	bus := newOptionBus()
	session := newFakeSession(t, "sess-a", "peer-1", 7)
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:          "crosstalk",
		OutputChannel: "graph.crosstalk.pcm",
		OutputProfile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: 16000, Channels: 1},
		Dial:          onceDial(session),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runStage(ctx, stage, bus)
	waitEpoch(t, stage)
	injectTone(t, session, 440, 300*time.Millisecond)
	events := waitPCM(t, bus, "graph.crosstalk.pcm", 16000/5, 12*time.Second)
	session.Close()
	cancel()
	<-errc
	sessionID := worker.CrosstalkSessionID("sess-a", "peer-1", 7)
	assertSourceEvents(t, events, "crosstalk", sessionID, qol.TypeAudioPCM)
	pcm := collectEventPCM(t, events)
	assertTone(t, pcm, 16000, 440, 180*time.Millisecond)
}

func TestCrosstalkSourcesEncodedForSTT(t *testing.T) {
	requireFFmpeg(t)
	bus := newOptionBus()
	session := newFakeSession(t, "sess-b", "peer-2", 1)
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:          "crosstalk",
		OutputChannel: qol.ChannelAudioInput,
		OutputProfile: media.Profile{Kind: media.ProfileOggOpus},
		Dial:          onceDial(session),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runStage(ctx, stage, bus)
	waitEpoch(t, stage)
	before := len(bus.snapshot(qol.ChannelAudioInput))
	injectTone(t, session, 523, 400*time.Millisecond)
	events := waitEncodedAfter(t, bus, qol.ChannelAudioInput, before, 1500, 12*time.Second)
	session.Close()
	cancel()
	<-errc
	if events[0].Type != qol.TypeAudioStream || events[0].Seq != 0 || len(events[0].Parents) != 0 {
		t.Fatalf("encoded source event: %#v", events[0])
	}
	var ogg []byte
	for _, event := range events {
		_, _, _, data, err := wire.UnmarshalAudioStream(event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		ogg = append(ogg, data...)
	}
	if len(ogg) < 4 || string(ogg[:4]) != "OggS" {
		t.Fatalf("expected ogg pages, got %d bytes", len(ogg))
	}
	assertTone(t, decodeWithSTT(t, ogg), 16000, 523, 180*time.Millisecond)
}

func TestCrosstalkSinksTTSPCM(t *testing.T) {
	requireFFmpeg(t)
	bus := newOptionBus()
	session := newFakeSession(t, "sess-c", "peer-3", 2)
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:         "crosstalk",
		InputChannel: qol.ChannelTTSAudio,
		Dial:         onceDial(session),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runStage(ctx, stage, bus)
	waitEpoch(t, stage)
	pcm := tonePCM(t, 880, 22050, 1, qol.SampleS16LE, 300*time.Millisecond)
	publishPCM(t, bus, qol.ChannelTTSAudio, "tts-session", 22050, pcm, true)
	frames := session.Collect(t, 8*time.Second)
	session.Close()
	cancel()
	<-errc
	if bus.opts.Group != "" {
		t.Fatalf("default group %q, want empty fan-out", bus.opts.Group)
	}
	assertTone(t, decodeFrames(t, frames, 16000), 16000, 880, 180*time.Millisecond)
}

func TestCrosstalkSinksEncodedCapture(t *testing.T) {
	requireFFmpeg(t)
	bus := newOptionBus()
	session := newFakeSession(t, "sess-d", "peer-4", 3)
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:         "crosstalk",
		InputChannel: qol.ChannelCaptureOutput,
		Dial:         onceDial(session),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runStage(ctx, stage, bus)
	waitEpoch(t, stage)
	ogg := encodedTone(t, 440, time.Second)
	mid := 2048
	if mid > len(ogg)/2 {
		mid = len(ogg) / 2
	}
	publishStream(t, bus, qol.ChannelCaptureOutput, "cap", 0, ogg[:mid], false)
	publishStream(t, bus, qol.ChannelCaptureOutput, "cap", 1, ogg[mid:], true)
	frames := session.Collect(t, 10*time.Second)
	session.Close()
	cancel()
	<-errc
	assertTone(t, decodeFrames(t, frames, 16000), 16000, 440, 400*time.Millisecond)
}

func TestCrosstalkDuplexIndependent(t *testing.T) {
	requireFFmpeg(t)
	bus := newOptionBus()
	session := newFakeSession(t, "sess-e", "peer-5", 4)
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:          "crosstalk",
		InputChannel:  "graph.sink",
		OutputChannel: "graph.source",
		OutputProfile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: 16000, Channels: 1},
		Dial:          onceDial(session),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runStage(ctx, stage, bus)
	waitEpoch(t, stage)
	done := make(chan struct{})
	go func() {
		defer close(done)
		injectTone(t, session, 440, 300*time.Millisecond)
	}()
	publishPCM(t, bus, "graph.sink", "producer", 22050, tonePCM(t, 880, 22050, 1, qol.SampleS16LE, 300*time.Millisecond), true)
	source := waitPCM(t, bus, "graph.source", 16000/8, 12*time.Second)
	frames := session.Collect(t, 10*time.Second)
	<-done
	session.Close()
	cancel()
	<-errc
	if source[0].SessionID == "producer" {
		t.Fatal("source events reused the sink session id")
	}
	assertTone(t, collectEventPCM(t, source), 16000, 440, 60*time.Millisecond)
	assertTone(t, decodeFrames(t, frames, 16000), 16000, 880, 60*time.Millisecond)
}

func TestCrosstalkFanOutDoesNotSteal(t *testing.T) {
	requireFFmpeg(t)
	bus := newOptionBus()
	var extra []qol.Event
	if _, err := bus.Subscribe(context.Background(), "graph.shared", qol.SubscribeOptions{Group: "record"}, func(_ context.Context, event qol.Event) error {
		extra = append(extra, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	session := newFakeSession(t, "sess-f", "peer-6", 5)
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:         "crosstalk",
		InputChannel: "graph.shared",
		Dial:         onceDial(session),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runStage(ctx, stage, bus)
	waitEpoch(t, stage)
	if bus.opts.Group != "" {
		t.Fatalf("crosstalk subscribed with group %q", bus.opts.Group)
	}
	publishPCM(t, bus, "graph.shared", "shared", 16000, tonePCM(t, 440, 16000, 1, qol.SampleS16LE, 200*time.Millisecond), true)
	frames := session.Collect(t, 8*time.Second)
	session.Close()
	cancel()
	<-errc
	if len(extra) == 0 {
		t.Fatal("second subscriber received no events")
	}
	if len(frames) == 0 {
		t.Fatal("crosstalk received no converted frames")
	}
}

func TestCrosstalkReconnectIsolatesEpochs(t *testing.T) {
	requireFFmpeg(t)
	bus := newOptionBus()
	first := newFakeSession(t, "sess-g", "peer-7", 1)
	second := newFakeSession(t, "sess-g", "peer-7", 2)
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:          "crosstalk",
		InputChannel:  "graph.sink",
		OutputChannel: "graph.source",
		OutputProfile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: 16000, Channels: 1},
		Reconnect:     true,
		MinBackoff:    time.Millisecond,
		Dial:          seqDial(first, second),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runStage(ctx, stage, bus)
	waitEpoch(t, stage)
	injectTone(t, first, 440, 250*time.Millisecond)
	firstEvents := waitPCM(t, bus, "graph.source", 16000/16, 12*time.Second)
	first.Close()
	deadline := time.Now().Add(8 * time.Second)
	for stage.Metrics().Epochs.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second epoch did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	publishPCM(t, bus, "graph.sink", "stale", 16000, tonePCM(t, 200, 16000, 1, qol.SampleS16LE, 80*time.Millisecond), false)
	injectTone(t, second, 880, 250*time.Millisecond)
	secondEvents := waitNewSession(t, bus, "graph.source", firstEvents[0].SessionID, 12*time.Second)
	second.Close()
	cancel()
	<-errc
	if firstEvents[0].SessionID == secondEvents[0].SessionID {
		t.Fatal("reconnect reused qol session id")
	}
	if firstEvents[0].ID == secondEvents[0].ID {
		t.Fatal("reconnect reused event id")
	}
	if secondEvents[0].Seq != 0 {
		t.Fatalf("new epoch seq %d, want 0", secondEvents[0].Seq)
	}
	assertTone(t, collectEventPCM(t, firstEvents), 16000, 440, 80*time.Millisecond)
	assertTone(t, collectEventPCM(t, secondEvents), 16000, 880, 80*time.Millisecond)
	ids := map[string]bool{}
	for _, event := range append(append([]qol.Event{}, firstEvents...), secondEvents...) {
		if ids[event.ID] {
			t.Fatalf("duplicate event id %s", event.ID)
		}
		ids[event.ID] = true
	}
}

func TestCrosstalkTerminalAuthDoesNotReconnect(t *testing.T) {
	authErr := &abc.AuthError{StatusCode: 401}
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:         "crosstalk",
		InputChannel: "graph.sink",
		Reconnect:    true,
		MinBackoff:   time.Millisecond,
		Dial: func(context.Context, worker.CrosstalkConfig) (worker.ABCSession, error) {
			return nil, authErr
		},
	})
	err := stage.Run(context.Background(), newOptionBus())
	if !errors.Is(err, authErr) {
		t.Fatalf("got %v, want auth error", err)
	}
	health := stage.Health()
	if health.Ready || health.Reason != "auth" {
		t.Fatalf("health %#v", health)
	}
}

func TestCrosstalkTerminalProtocolDoesNotReconnect(t *testing.T) {
	protoErr := &abc.ProtocolError{Reason: "bad hello"}
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:          "crosstalk",
		OutputChannel: "graph.source",
		OutputProfile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: 16000, Channels: 1},
		Reconnect:     true,
		Dial: func(context.Context, worker.CrosstalkConfig) (worker.ABCSession, error) {
			return nil, protoErr
		},
	})
	err := stage.Run(context.Background(), newOptionBus())
	if !errors.Is(err, protoErr) {
		t.Fatalf("got %v, want protocol error", err)
	}
	if got := stage.Health(); got.Ready || got.Reason != "protocol" {
		t.Fatalf("health %#v", got)
	}
}

func TestCrosstalkRetriesTransientDialThenReady(t *testing.T) {
	requireFFmpeg(t)
	session := newFakeSession(t, "sess-retry", "peer-r", 3)
	var attempts atomic.Uint32
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:          "crosstalk",
		OutputChannel: "graph.source",
		OutputProfile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: 16000, Channels: 1},
		Reconnect:     true,
		MinBackoff:    time.Millisecond,
		Dial: func(ctx context.Context, cfg worker.CrosstalkConfig) (worker.ABCSession, error) {
			if attempts.Add(1) == 1 {
				return nil, errors.New("temporary dial failure")
			}
			return session, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runStage(ctx, stage, newOptionBus())
	deadline := time.Now().Add(3 * time.Second)
	for !stage.Health().Ready {
		if time.Now().After(deadline) {
			t.Fatalf("never became ready: %#v attempts=%d", stage.Health(), attempts.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if attempts.Load() < 2 {
		t.Fatal("transient failure was not retried")
	}
	if stage.Health().Reason != "ready" {
		t.Fatalf("health %#v", stage.Health())
	}
	cancel()
	<-errc
}

func TestCrosstalkUnassignedDoesNotPublish(t *testing.T) {
	session := &waitSession{welcome: worker.ABCWelcome{PeerID: "peer-u", Epoch: 9}, done: make(chan struct{})}
	t.Cleanup(func() { _ = session.Close() })
	bus := newOptionBus()
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:          "crosstalk",
		OutputChannel: "graph.source",
		OutputProfile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: 16000, Channels: 1},
		Dial:          onceDial(session),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runStage(ctx, stage, bus)
	deadline := time.Now().Add(2 * time.Second)
	for stage.Health().Reason != "unassigned" {
		if time.Now().After(deadline) {
			t.Fatalf("health %#v", stage.Health())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if stage.Metrics().Epochs.Load() != 0 {
		t.Fatal("unassigned session started an epoch")
	}
	if events := bus.snapshot("graph.source"); len(events) != 0 {
		t.Fatalf("published %d events while unassigned", len(events))
	}
	if stage.Health().Ready {
		t.Fatal("unassigned reported ready")
	}
	cancel()
	<-errc
}

func TestCrosstalkOverflowAndCancel(t *testing.T) {
	requireFFmpeg(t)
	bus := newOptionBus()
	session := newFakeSession(t, "sess-h", "peer-8", 8)
	stage := mustCrosstalk(t, worker.CrosstalkConfig{
		Name:         "crosstalk",
		InputChannel: "graph.sink",
		Queue:        1,
		Overflow:     media.OverflowFail,
		Dial:         onceDial(session),
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := runStage(ctx, stage, bus)
	waitEpoch(t, stage)
	pcm := tonePCM(t, 440, 16000, 1, qol.SampleS16LE, 20*time.Millisecond)
	for i := 0; i < 8; i++ {
		publishPCMSeq(t, bus, "graph.sink", "burst", uint64(i), 16000, pcm, false)
	}
	cancel()
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("stage did not return after cancel")
	}
	_ = session.Close()
	if stage.Metrics().Dropped.Load() == 0 && stage.Metrics().SinkErrors.Load() == 0 {
		t.Fatal("expected overflow or sink pressure to be observed")
	}
}

type optionBus struct {
	*memoryBus
	mu   sync.Mutex
	opts qol.SubscribeOptions
}

func newOptionBus() *optionBus {
	return &optionBus{memoryBus: newMemoryBus()}
}

func (b *optionBus) Subscribe(ctx context.Context, channel string, opts qol.SubscribeOptions, handler qol.Handler) (qol.Subscription, error) {
	b.mu.Lock()
	b.opts = opts
	b.mu.Unlock()
	return b.memoryBus.Subscribe(ctx, channel, opts, handler)
}

func (b *optionBus) snapshot(channel string) []qol.Event {
	b.memoryBus.mu.Lock()
	defer b.memoryBus.mu.Unlock()
	return append([]qol.Event(nil), b.memoryBus.events[channel]...)
}

func mustCrosstalk(t *testing.T, cfg worker.CrosstalkConfig) *worker.Crosstalk {
	t.Helper()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	stage, err := worker.NewCrosstalk(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return stage
}

func runStage(ctx context.Context, stage qol.Stage, bus qol.Bus) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- stage.Run(ctx, bus) }()
	return errc
}

func waitEpoch(t *testing.T, stage *worker.Crosstalk) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for stage.Metrics().Epochs.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("epoch did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
}

func onceDial(session worker.ABCSession) worker.ABCDialer {
	return seqDial(session)
}

func seqDial(sessions ...worker.ABCSession) worker.ABCDialer {
	var mu sync.Mutex
	i := 0
	return func(ctx context.Context, _ worker.CrosstalkConfig) (worker.ABCSession, error) {
		mu.Lock()
		if i >= len(sessions) {
			mu.Unlock()
			<-ctx.Done()
			return nil, ctx.Err()
		}
		session := sessions[i]
		i++
		mu.Unlock()
		return session, nil
	}
}

type fakeTrack struct {
	codec media.Codec
	read  func() (*rtp.Packet, error)
}

func (t fakeTrack) ReadRTP() (*rtp.Packet, error) { return t.read() }
func (t fakeTrack) Codec() media.Codec            { return t.codec }

type fakeSession struct {
	welcome ABCWelcomeValue
	codec   media.Codec
	pair    *pionPair
	mu      sync.Mutex
	onTrack func(worker.ABCTrack)
	onClose func(string)
	pending []worker.ABCTrack
	done    chan struct{}
	once    sync.Once
}

type ABCWelcomeValue = worker.ABCWelcome

func newFakeSession(t *testing.T, assigned, peer string, epoch uint64) *fakeSession {
	t.Helper()
	pair := newPionPair(t)
	session := &fakeSession{
		welcome: worker.ABCWelcome{PeerID: peer, AssignedSessionID: assigned, Epoch: epoch},
		codec:   media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
		pair:    pair,
		done:    make(chan struct{}),
	}
	t.Cleanup(func() { _ = session.Close() })
	pair.drainRemotes()
	bind := func(track *webrtc.TrackRemote) {
		session.dispatch(fakeTrack{codec: session.codec, read: func() (*rtp.Packet, error) {
			packet, _, err := track.ReadRTP()
			return packet, err
		}})
	}
	if remote := pair.abcRemoteTrack(); remote != nil {
		bind(remote)
		return session
	}
	go func() {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if remote := pair.abcRemoteTrack(); remote != nil {
				bind(remote)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return session
}

func (s *fakeSession) Welcome() worker.ABCWelcome { return s.welcome }
func (s *fakeSession) NegotiatedCodec() (media.Codec, bool) {
	return s.codec, true
}
func (s *fakeSession) WriteRTP(packet *rtp.Packet) error {
	return s.pair.abcSend.WriteRTP(packet)
}
func (s *fakeSession) OnTrack(fn func(worker.ABCTrack)) {
	s.mu.Lock()
	s.onTrack = fn
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	for _, track := range pending {
		fn(track)
	}
}
func (s *fakeSession) OnClose(fn func(string)) {
	s.mu.Lock()
	s.onClose = fn
	s.mu.Unlock()
}
func (s *fakeSession) Done() <-chan struct{} { return s.done }
func (s *fakeSession) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		fn := s.onClose
		s.mu.Unlock()
		if fn != nil {
			fn("closed")
		}
		s.pair.Close()
		close(s.done)
	})
	return nil
}
func (s *fakeSession) Err() error { return nil }

type waitSession struct {
	welcome worker.ABCWelcome
	done    chan struct{}
	once    sync.Once
}

func (s *waitSession) Welcome() worker.ABCWelcome                 { return s.welcome }
func (s *waitSession) NegotiatedCodec() (media.Codec, bool)       { return media.Codec{}, false }
func (s *waitSession) WriteRTP(*rtp.Packet) error                 { return nil }
func (s *waitSession) OnTrack(func(worker.ABCTrack))              {}
func (s *waitSession) OnClose(func(string))                       {}
func (s *waitSession) Done() <-chan struct{}                      { return s.done }
func (s *waitSession) Err() error                                 { return nil }
func (s *waitSession) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

func (s *fakeSession) dispatch(track worker.ABCTrack) {
	s.mu.Lock()
	fn := s.onTrack
	if fn == nil {
		s.pending = append(s.pending, track)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	fn(track)
}

func (s *fakeSession) Collect(t *testing.T, wait time.Duration) []media.Frame {
	t.Helper()
	s.pair.waitConnected(t)
	remote := s.pair.waitServerRemote(t, wait)
	var frames []media.Frame
	deadline := time.Now().Add(wait)
	idle := 0
	for time.Now().Before(deadline) {
		_ = remote.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		packet, _, err := remote.ReadRTP()
		if err != nil {
			if len(frames) > 0 {
				idle++
				if idle > 4 {
					return frames
				}
			}
			continue
		}
		idle = 0
		if len(packet.Payload) == 0 {
			continue
		}
		frames = append(frames, media.Frame{
			Payload:     append([]byte(nil), packet.Payload...),
			Sequence:    packet.SequenceNumber,
			Timestamp:   packet.Timestamp,
			Marker:      packet.Marker,
			PayloadType: packet.PayloadType,
			SSRC:        packet.SSRC,
		})
		if len(frames) >= 30 {
			return frames
		}
	}
	if len(frames) == 0 {
		t.Fatal("timed out collecting sink rtp")
	}
	return frames
}

func injectTone(t *testing.T, session *fakeSession, freq float64, duration time.Duration) {
	t.Helper()
	session.pair.waitConnected(t)
	for _, frame := range opusFrames(t, freq, duration) {
		if err := session.pair.serverSend.WriteRTP(media.PackRTP(frame)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type pionPair struct {
	mu                      sync.Mutex
	abcPC, serverPC         *webrtc.PeerConnection
	abcSend, serverSend     *webrtc.TrackLocalStaticRTP
	abcRemote, serverRemote *webrtc.TrackRemote
}

func newPionPair(t *testing.T) *pionPair {
	t.Helper()
	api := pionAPI()
	abcPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	serverPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	abcSend, err := webrtc.NewTrackLocalStaticRTP(codec, "abc", "qol")
	if err != nil {
		t.Fatal(err)
	}
	serverSend, err := webrtc.NewTrackLocalStaticRTP(codec, "monitor", "crosstalk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := abcPC.AddTransceiverFromTrack(abcSend, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		t.Fatal(err)
	}
	if _, err := serverPC.AddTransceiverFromTrack(serverSend, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		t.Fatal(err)
	}
	pair := &pionPair{abcPC: abcPC, serverPC: serverPC, abcSend: abcSend, serverSend: serverSend}
	abcPC.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		pair.mu.Lock()
		pair.abcRemote = track
		pair.mu.Unlock()
	})
	serverPC.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		pair.mu.Lock()
		pair.serverRemote = track
		pair.mu.Unlock()
	})
	abcPC.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			_ = serverPC.AddICECandidate(candidate.ToJSON())
		}
	})
	serverPC.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			_ = abcPC.AddICECandidate(candidate.ToJSON())
		}
	})
	offer, err := abcPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := abcPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-webrtc.GatheringCompletePromise(abcPC):
	case <-time.After(5 * time.Second):
		t.Fatal("ice gather timeout")
	}
	if err := serverPC.SetRemoteDescription(*abcPC.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := serverPC.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := serverPC.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-webrtc.GatheringCompletePromise(serverPC):
	case <-time.After(5 * time.Second):
		t.Fatal("ice gather timeout")
	}
	if err := abcPC.SetRemoteDescription(*serverPC.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	pair.waitConnected(t)
	pair.primeTracks(t)
	pair.drainRemotes()
	return pair
}

func (p *pionPair) abcRemoteTrack() *webrtc.TrackRemote {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.abcRemote
}

func (p *pionPair) drainRemotes() {
	deadline := time.Now().Add(400 * time.Millisecond)
	idle := 0
	var abc, server *webrtc.TrackRemote
	for time.Now().Before(deadline) {
		p.mu.Lock()
		abc = p.abcRemote
		server = p.serverRemote
		p.mu.Unlock()
		progress := false
		if abc != nil {
			_ = abc.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			if _, _, err := abc.ReadRTP(); err == nil {
				progress = true
			}
		}
		if server != nil {
			_ = server.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			if _, _, err := server.ReadRTP(); err == nil {
				progress = true
			}
		}
		if progress {
			idle = 0
			continue
		}
		idle++
		if idle >= 3 {
			break
		}
	}
	clearRemoteDeadlines(abc, server)
}

func clearRemoteDeadlines(tracks ...*webrtc.TrackRemote) {
	for _, track := range tracks {
		if track != nil {
			_ = track.SetReadDeadline(time.Time{})
		}
	}
}

func (p *pionPair) primeTracks(t *testing.T) {
	t.Helper()
	frames := opusFrames(t, 0, 80*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	i := 0
	for time.Now().Before(deadline) {
		p.mu.Lock()
		haveABC := p.abcRemote != nil
		haveServer := p.serverRemote != nil
		p.mu.Unlock()
		if haveABC && haveServer {
			return
		}
		frame := frames[i%len(frames)]
		i++
		pkt := media.PackRTP(frame)
		pkt.SequenceNumber = uint16(i)
		pkt.Timestamp = uint32(i) * 960
		_ = p.abcSend.WriteRTP(pkt)
		_ = p.serverSend.WriteRTP(pkt)
		time.Sleep(20 * time.Millisecond)
	}
	p.mu.Lock()
	haveABC := p.abcRemote != nil
	haveServer := p.serverRemote != nil
	p.mu.Unlock()
	t.Fatalf("tracks did not start abc=%v server=%v", haveABC, haveServer)
}

func (p *pionPair) waitConnected(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		abcOK := connected(p.abcPC)
		serverOK := connected(p.serverPC)
		if abcOK && serverOK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ice connect timeout abc=%s/%s server=%s/%s",
		p.abcPC.ConnectionState(), p.abcPC.ICEConnectionState(),
		p.serverPC.ConnectionState(), p.serverPC.ICEConnectionState())
}

func (p *pionPair) waitServerRemote(t *testing.T, wait time.Duration) *webrtc.TrackRemote {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		remote := p.serverRemote
		p.mu.Unlock()
		if remote != nil {
			return remote
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server remote track never arrived")
	return nil
}

func connected(pc *webrtc.PeerConnection) bool {
	switch pc.ConnectionState() {
	case webrtc.PeerConnectionStateConnected:
		return true
	}
	switch pc.ICEConnectionState() {
	case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
		return true
	default:
		return false
	}
}

func (p *pionPair) Close() {
	if p.abcPC != nil {
		_ = p.abcPC.Close()
	}
	if p.serverPC != nil {
		_ = p.serverPC.Close()
	}
}

func pionAPI() *webrtc.API {
	var settings webrtc.SettingEngine
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetInterfaceFilter(func(name string) bool { return name == "lo" })
	settings.SetNAT1To1IPs([]string{"127.0.0.1"}, webrtc.ICECandidateTypeHost)
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings))
}

func publishPCM(t *testing.T, bus qol.Bus, channel, sessionID string, rate int, pcm []byte, end bool) {
	t.Helper()
	publishPCMSeq(t, bus, channel, sessionID, 0, rate, pcm, end)
}

func publishPCMSeq(t *testing.T, bus qol.Bus, channel, sessionID string, seq uint64, rate int, pcm []byte, end bool) {
	t.Helper()
	payload, err := wire.MarshalAudio(qol.Audio{Format: qol.PCMFormat{SampleRate: rate, Channels: 1, Format: qol.SampleS16LE}, Data: pcm})
	if err != nil {
		t.Fatal(err)
	}
	eventType := qol.TypeAudioPCM
	if end {
		eventType = qol.TypeAudioPCMFinal
	}
	if err := bus.Publish(context.Background(), qol.Event{
		ID: sessionID + "-" + string(rune('a'+seq)), SessionID: sessionID, Channel: channel, Type: eventType,
		Seq: seq, Span: qol.Span{End: qol.MediaTime(time.Second)}, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
}

func publishStream(t *testing.T, bus qol.Bus, channel, sessionID string, seq uint64, data []byte, end bool) {
	t.Helper()
	payload, err := wire.MarshalAudioStream(media.MediaTypeOggOpus, "capture", end, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), qol.Event{
		ID: sessionID + "-" + string(rune('a'+seq)), SessionID: sessionID, Channel: channel, Type: qol.TypeAudioStream,
		Seq: seq, Span: qol.Span{End: 1}, ProducedAt: time.Now(), Encoding: qol.EncodingProtobuf, Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
}

func waitEvents(t *testing.T, bus *optionBus, channel string, min int, wait time.Duration) []qol.Event {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		events := bus.snapshot(channel)
		live := 0
		for _, event := range events {
			if !isEOSEvent(event) {
				live++
			}
		}
		if live >= min {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d events on %s, got %d", min, channel, len(events))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitPCM(t *testing.T, bus *optionBus, channel string, minBytes int, wait time.Duration) []qol.Event {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		events := bus.snapshot(channel)
		got := 0
		for _, event := range events {
			if isEOSEvent(event) {
				continue
			}
			audio, err := wire.UnmarshalAudio(event.Payload)
			if err != nil {
				continue
			}
			got += len(audio.Data)
		}
		if got >= minBytes {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d pcm bytes on %s, got %d", minBytes, channel, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitEncoded(t *testing.T, bus *optionBus, channel string, minBytes int, wait time.Duration) []qol.Event {
	t.Helper()
	return waitEncodedAfter(t, bus, channel, 0, minBytes, wait)
}

func waitEncodedAfter(t *testing.T, bus *optionBus, channel string, skip, minBytes int, wait time.Duration) []qol.Event {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		all := bus.snapshot(channel)
		if skip > len(all) {
			skip = len(all)
		}
		events := all[skip:]
		got := 0
		for _, event := range events {
			if isEOSEvent(event) {
				continue
			}
			_, _, _, data, err := wire.UnmarshalAudioStream(event.Payload)
			if err != nil {
				continue
			}
			got += len(data)
		}
		if got >= minBytes {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d encoded bytes on %s, got %d", minBytes, channel, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitNewSession(t *testing.T, bus *optionBus, channel, oldSession string, wait time.Duration) []qol.Event {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		events := bus.snapshot(channel)
		var next []qol.Event
		for _, event := range events {
			if event.SessionID != oldSession && !isEOSEvent(event) {
				next = append(next, event)
			}
		}
		if len(next) > 0 {
			return next
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for new epoch events")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isEOSEvent(event qol.Event) bool {
	if event.Type == qol.TypeAudioPCMFinal {
		audio, err := wire.UnmarshalAudio(event.Payload)
		return err == nil && len(audio.Data) <= 2
	}
	if event.Type == qol.TypeAudioStream {
		_, _, end, data, err := wire.UnmarshalAudioStream(event.Payload)
		return err == nil && end && len(data) <= 1
	}
	return false
}

func assertSourceEvents(t *testing.T, events []qol.Event, stage, sessionID, eventType string) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no source events")
	}
	seen := map[string]bool{}
	var last qol.MediaTime
	live := 0
	for i, event := range events {
		if event.SessionID != sessionID {
			t.Fatalf("session %s, want %s", event.SessionID, sessionID)
		}
		if event.ID != deterministicEventID(stage, sessionID, event.Seq) {
			t.Fatalf("event id %s does not match seq %d", event.ID, event.Seq)
		}
		if seen[event.ID] {
			t.Fatalf("duplicate id %s", event.ID)
		}
		seen[event.ID] = true
		if len(event.Parents) != 0 {
			t.Fatalf("source event has parents: %#v", event.Parents)
		}
		if event.Span.End < event.Span.Start {
			t.Fatalf("span %#v", event.Span)
		}
		if !isEOSEvent(event) {
			if event.Type != eventType && event.Type != qol.TypeAudioPCMFinal {
				t.Fatalf("type %s", event.Type)
			}
			if live == 0 && event.Seq != 0 {
				t.Fatalf("first live seq %d", event.Seq)
			}
			if last > 0 && event.Span.Start < last {
				t.Fatalf("non-monotonic span at %d", i)
			}
			last = event.Span.End
			live++
		}
	}
	if live == 0 {
		t.Fatal("only eos events arrived")
	}
}

func deterministicEventID(stage, sessionID string, seq uint64) string {
	hash := sha256.New()
	for _, part := range []string{stage, sessionID, strconv.FormatUint(seq, 10)} {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func collectEventPCM(t *testing.T, events []qol.Event) []byte {
	t.Helper()
	var out []byte
	for _, event := range events {
		if isEOSEvent(event) {
			continue
		}
		audio, err := wire.UnmarshalAudio(event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, audio.Data...)
	}
	return out
}

func decodeFrames(t *testing.T, frames []media.Frame, rate int) []byte {
	t.Helper()
	in, err := media.NewInbound(media.InboundConfig{
		Codec:   media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
		Profile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: rate, Channels: 1},
		Queue:   64,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		if err := in.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.WriteFrame(media.Frame{End: true}); err != nil {
		t.Fatal(err)
	}
	var out []byte
	deadline := time.After(6 * time.Second)
	for {
		select {
		case chunk, ok := <-in.Chunks():
			if !ok {
				_ = in.Close()
				return out
			}
			out = append(out, chunk.Data...)
			if chunk.End {
				_ = in.Close()
				return out
			}
		case <-deadline:
			_ = in.Close()
			if len(out) == 0 {
				t.Fatal("timed out decoding frames")
			}
			return out
		}
	}
}

func decodeWithSTT(t *testing.T, ogg []byte) []byte {
	t.Helper()
	decoder := media.NewDecoder("ffmpeg")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	input, output, wait, err := decoder.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(ogg); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil && len(data) == 0 {
		t.Fatalf("stt decode failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("stt decode produced no pcm")
	}
	return data
}

func opusFrames(t *testing.T, freq float64, duration time.Duration) []media.Frame {
	t.Helper()
	out, err := media.NewOutbound(media.OutboundConfig{Codec: media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1}, Queue: 16})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := out.Start(ctx); err != nil {
		t.Fatal(err)
	}
	pcm := tonePCM(t, freq, 48000, 1, qol.SampleS16LE, duration)
	if err := out.WritePCM(qol.Audio{Format: qol.PCMFormat{SampleRate: 48000, Channels: 1, Format: qol.SampleS16LE}, Data: pcm}, qol.Span{}, true); err != nil {
		t.Fatal(err)
	}
	var frames []media.Frame
	deadline := time.After(6 * time.Second)
	for {
		select {
		case frame, ok := <-out.Frames():
			if !ok {
				_ = out.Close()
				return frames
			}
			if frame.End {
				_ = out.Close()
				return frames
			}
			if len(frame.Payload) > 0 {
				frames = append(frames, frame)
			}
		case <-deadline:
			_ = out.Close()
			if len(frames) == 0 {
				t.Fatal("no opus frames")
			}
			return frames
		}
	}
}

func encodedTone(t *testing.T, freq float64, duration time.Duration) []byte {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "libopus", "-page_duration", "20000", "-f", "ogg", "pipe:1")
	if freq != 440 || duration != time.Second {
		cmd = exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency="+itoa(int(freq))+":duration="+ftoa(duration.Seconds()), "-c:a", "libopus", "-page_duration", "20000", "-f", "ogg", "pipe:1")
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func tonePCM(t *testing.T, freq float64, rate, channels int, format qol.SampleFormat, duration time.Duration) []byte {
	t.Helper()
	samples := int(duration.Seconds() * float64(rate))
	out := make([]byte, samples*channels*2)
	for i := 0; i < samples; i++ {
		value := int16(math.Sin(2*math.Pi*freq*float64(i)/float64(rate)) * 20000)
		for ch := 0; ch < channels; ch++ {
			binary.LittleEndian.PutUint16(out[(i*channels+ch)*2:], uint16(value))
		}
	}
	_ = format
	return out
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
}

func assertTone(t *testing.T, pcm []byte, rate int, freq float64, minDur time.Duration) {
	t.Helper()
	if len(pcm) < 64 {
		t.Fatalf("pcm too short: %d", len(pcm))
	}
	samples := make([]float64, len(pcm)/2)
	energy := 0.0
	for i := range samples {
		samples[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		energy += samples[i] * samples[i]
	}
	if energy == 0 {
		t.Fatal("decoded audio is silent")
	}
	got := dominantFreq(samples, rate)
	if math.Abs(got-freq) > freq*0.15 && math.Abs(got-freq) > 50 {
		t.Fatalf("dominant frequency %.1f, want %.1f", got, freq)
	}
	dur := time.Duration(len(samples)) * time.Second / time.Duration(rate)
	if dur < minDur {
		t.Fatalf("duration %s below %s", dur, minDur)
	}
}

func dominantFreq(samples []float64, rate int) float64 {
	n := 1
	for n < len(samples) {
		n <<= 1
	}
	if n > 8192 {
		n = 8192
	}
	if len(samples) > n {
		samples = samples[:n]
	}
	spec := make([]complex128, n)
	for i, sample := range samples {
		spec[i] = complex(sample, 0)
	}
	fft(spec)
	bestBin := 1
	bestMag := 0.0
	for i := 1; i < n/2; i++ {
		mag := cmplx.Abs(spec[i])
		if mag > bestMag {
			bestMag = mag
			bestBin = i
		}
	}
	return float64(bestBin) * float64(rate) / float64(n)
}

func fft(a []complex128) {
	n := len(a)
	for i, j := 0, 0; i < n; i++ {
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
		mask := n / 2
		for j >= mask && mask > 0 {
			j -= mask
			mask /= 2
		}
		j += mask
	}
	for size := 2; size <= n; size <<= 1 {
		half := size / 2
		step := -2 * math.Pi / float64(size)
		for i := 0; i < n; i += size {
			for j := 0; j < half; j++ {
				angle := complex(0, step*float64(j))
				w := cmplx.Exp(angle) * a[i+j+half]
				a[i+j+half] = a[i+j] - w
				a[i+j] += w
			}
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func ftoa(v float64) string {
	return itoa(int(v)) + "." + itoa(int(v*1000)%1000)
}
