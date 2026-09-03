package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/media"
	"github.com/aleksclark/qol/internal/wire"
)

type ABCWelcome struct {
	PeerID            string
	AssignedSessionID string
	Epoch             uint64
}

type ABCTrack interface {
	ReadRTP() (*rtp.Packet, error)
	Codec() media.Codec
}

type ABCSession interface {
	Welcome() ABCWelcome
	NegotiatedCodec() (media.Codec, bool)
	WriteRTP(*rtp.Packet) error
	OnTrack(func(ABCTrack))
	OnClose(func(string))
	Done() <-chan struct{}
	Close() error
	Err() error
}

type ABCDialer func(context.Context, CrosstalkConfig) (ABCSession, error)

type CrosstalkConfig struct {
	Name          string
	ServerURL     string
	Token         string
	ClientName    string
	InputChannel  string
	OutputChannel string
	OutputProfile media.Profile
	Group         string
	Competing     bool
	Queue         int
	Overflow      media.OverflowPolicy
	FFmpeg        string
	Reconnect     bool
	MinBackoff    time.Duration
	MaxBackoff    time.Duration
	DisableMDNS   bool
	DisableSTUN   bool
	Dial          ABCDialer
	Now           func() time.Time
	Logger        *slog.Logger
}

type CrosstalkMetrics struct {
	SourceFrames     atomic.Uint64
	SinkFrames       atomic.Uint64
	Dropped          atomic.Uint64
	RejectedSessions atomic.Uint64
	Duplicates       atomic.Uint64
	SourceErrors     atomic.Uint64
	SinkErrors       atomic.Uint64
	Epochs           atomic.Uint64
	reason           atomic.Value
}

type CrosstalkHealth struct {
	Ready     bool
	Reason    string
	SessionID string
	Assigned  string
	Peer      string
	Epoch     uint64
}

func (m *CrosstalkMetrics) Reason() string {
	value, _ := m.reason.Load().(string)
	return value
}

func (m *CrosstalkMetrics) setReason(reason string) {
	if reason != "" {
		m.reason.Store(reason)
	}
}

type Crosstalk struct {
	cfg     CrosstalkConfig
	spec    qol.StageSpec
	metrics *CrosstalkMetrics
	now     func() time.Time
	logger  *slog.Logger

	mu             sync.Mutex
	sinkRTMu       sync.Mutex
	activeSession  string
	nextSeq        uint64
	outbound       *media.Outbound
	epochCtx       context.Context
	epochABC       ABCSession
	epochSessionID string
	epochReady     bool
	sourceSeq      uint64
	eosSent        bool
	sinkSeq        uint16
	health         CrosstalkHealth
	epochErrc      chan error
}

func NewCrosstalk(cfg CrosstalkConfig) (*Crosstalk, error) {
	if cfg.Name == "" {
		cfg.Name = "crosstalk"
	}
	if cfg.InputChannel == "" && cfg.OutputChannel == "" {
		return nil, errors.New("crosstalk requires an input channel, output channel, or both")
	}
	if cfg.InputChannel != "" && cfg.InputChannel == cfg.OutputChannel {
		return nil, errors.New("crosstalk input and output channels must differ")
	}
	if cfg.OutputChannel != "" {
		if err := media.ValidateProfile(cfg.OutputProfile); err != nil {
			return nil, err
		}
	}
	if cfg.Dial == nil {
		if cfg.ServerURL == "" || cfg.Token == "" {
			return nil, errors.New("crosstalk requires a server URL and token")
		}
		cfg.Dial = defaultABCDial
	}
	if cfg.Queue <= 0 {
		cfg.Queue = 64
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	stage := &Crosstalk{cfg: cfg, metrics: &CrosstalkMetrics{}, now: cfg.Now, logger: logger}
	spec := qol.StageSpec{Name: cfg.Name}
	if cfg.InputChannel != "" {
		spec.Inputs = []qol.Port{{Name: "sink", Channel: cfg.InputChannel, Types: []string{qol.TypeAudioStream, qol.TypeAudioPCM, qol.TypeAudioPCMFinal}}}
	}
	if cfg.OutputChannel != "" {
		types := []string{qol.TypeAudioPCM, qol.TypeAudioPCMFinal}
		if cfg.OutputProfile.Kind == media.ProfileOggOpus {
			types = []string{qol.TypeAudioStream}
		}
		spec.Outputs = []qol.Port{{Name: "source", Channel: cfg.OutputChannel, Types: types}}
	}
	stage.spec = spec
	return stage, nil
}

func (c *Crosstalk) Spec() qol.StageSpec { return c.spec }

func (c *Crosstalk) Metrics() *CrosstalkMetrics { return c.metrics }

func (c *Crosstalk) Health() CrosstalkHealth {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health
}

func (c *Crosstalk) SubscribeOptions() qol.SubscribeOptions {
	opts := qol.SubscribeOptions{Replay: qol.ReplayPosition{Policy: qol.ReplayLive}}
	if c.cfg.Competing {
		opts.Group = c.cfg.Group
		if opts.Group == "" {
			opts.Group = "qol-" + c.cfg.Name
		}
	}
	return opts
}

func CrosstalkSessionID(assigned, peer string, epoch uint64) string {
	if assigned == "" {
		assigned = "unassigned"
	}
	if peer == "" {
		peer = "peer"
	}
	return assigned + "/" + peer + "/" + strconv.FormatUint(epoch, 10)
}

func (c *Crosstalk) Run(ctx context.Context, bus qol.Bus) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.setHealth(CrosstalkHealth{Reason: "starting"})
	var subscription qol.Subscription
	if c.cfg.InputChannel != "" {
		sub, err := bus.Subscribe(ctx, c.cfg.InputChannel, c.SubscribeOptions(), func(eventContext context.Context, event qol.Event) error {
			err := c.handleSink(eventContext, event)
			if err != nil {
				c.failEpoch(err)
				// The subscription outlives an epoch. Returning the error would
				// terminate delivery and leave a reconnected epoch half-live.
				return nil
			}
			return nil
		})
		if err != nil {
			c.setHealth(CrosstalkHealth{Reason: "nats-subscribe"})
			return err
		}
		subscription = sub
		defer subscription.Close()
	}
	backoff := c.cfg.MinBackoff
	for {
		if err := ctx.Err(); err != nil {
			c.setHealth(CrosstalkHealth{Reason: "canceled"})
			return nil
		}
		c.setHealth(CrosstalkHealth{Reason: "connecting"})
		session, err := c.cfg.Dial(ctx, c.cfg)
		if err != nil {
			if ctx.Err() != nil {
				c.setHealth(CrosstalkHealth{Reason: "canceled"})
				return nil
			}
			if isTerminalABCError(err) || !c.cfg.Reconnect {
				c.metrics.setReason(terminalReason(err))
				c.setHealth(CrosstalkHealth{Reason: c.metrics.Reason()})
				return err
			}
			c.logger.Error("crosstalk dial failed", "stage", c.cfg.Name, "err", err)
			c.setHealth(CrosstalkHealth{Reason: "retrying"})
			if !c.waitBackoff(ctx, &backoff) {
				c.setHealth(CrosstalkHealth{Reason: "canceled"})
				return nil
			}
			continue
		}
		backoff = 0
		err = c.runEpoch(ctx, bus, session)
		_ = session.Close()
		if ctx.Err() != nil {
			c.setHealth(CrosstalkHealth{Reason: "canceled"})
			return nil
		}
		if isTerminalABCError(err) || !c.cfg.Reconnect {
			if c.metrics.Reason() == "" {
				c.metrics.setReason(terminalReason(err))
			}
			c.setHealth(CrosstalkHealth{Reason: c.metrics.Reason()})
			return err
		}
		if err != nil {
			c.logger.Error("crosstalk epoch ended", "stage", c.cfg.Name, "err", err, "reason", c.metrics.Reason())
		}
		c.setHealth(CrosstalkHealth{Reason: "retrying"})
		if !c.waitBackoff(ctx, &backoff) {
			c.setHealth(CrosstalkHealth{Reason: "canceled"})
			return nil
		}
	}
}

func (c *Crosstalk) runEpoch(ctx context.Context, bus qol.Bus, session ABCSession) error {
	welcome := session.Welcome()
	if welcome.AssignedSessionID == "" {
		c.setHealth(CrosstalkHealth{Reason: "unassigned", Peer: welcome.PeerID, Epoch: welcome.Epoch})
		c.logger.Info("crosstalk waiting for assignment", "stage", c.cfg.Name, "peer", welcome.PeerID, "epoch", welcome.Epoch)
		select {
		case <-ctx.Done():
			return nil
		case <-session.Done():
			return session.Err()
		}
	}
	sessionID := CrosstalkSessionID(welcome.AssignedSessionID, welcome.PeerID, welcome.Epoch)
	c.metrics.Epochs.Add(1)
	c.logger.Info("crosstalk epoch started",
		"stage", c.cfg.Name,
		"qol_session", sessionID,
		"assigned_session", welcome.AssignedSessionID,
		"peer", welcome.PeerID,
		"epoch", welcome.Epoch,
		"input", c.cfg.InputChannel,
		"output", c.cfg.OutputChannel,
	)
	codec, ok := session.NegotiatedCodec()
	if !ok {
		codec = media.Codec{MimeType: media.MimeTypeOpus, ClockRate: media.OpusClockRate, Channels: 1}
	}
	if err := media.ValidateCodec(codec); err != nil {
		c.metrics.setReason("unsupported-codec")
		c.finishEpoch(ctx, bus, sessionID, "unsupported-codec")
		return err
	}

	epochCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 4)
	c.mu.Lock()
	c.epochErrc = errc
	c.epochCtx = epochCtx
	c.epochABC = session
	c.mu.Unlock()
	var inbound *media.Inbound
	var workers sync.WaitGroup
	if c.cfg.OutputChannel != "" {
		converter, err := media.NewInbound(media.InboundConfig{FFmpeg: c.cfg.FFmpeg, Codec: codec, Profile: c.cfg.OutputProfile, Queue: c.cfg.Queue, Overflow: c.cfg.Overflow})
		if err != nil {
			c.finishEpoch(ctx, bus, sessionID, "inbound-init")
			return err
		}
		inbound = converter
		if err := inbound.Start(epochCtx); err != nil {
			c.metrics.setReason("conversion-failure")
			_ = inbound.Close()
			c.finishEpoch(ctx, bus, sessionID, "inbound-start")
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := c.publishInbound(epochCtx, bus, sessionID, inbound); err != nil && epochCtx.Err() == nil {
				c.metrics.SourceErrors.Add(1)
				c.failEpoch(converterError("source", err))
			}
		}()
	}
	if c.cfg.InputChannel != "" {
		c.mu.Lock()
		c.epochSessionID = sessionID
		c.epochReady = true
		c.activeSession = ""
		c.nextSeq = 0
		c.sourceSeq = 0
		c.eosSent = false
		c.sinkSeq = 0
		err := c.startOutboundLocked()
		c.mu.Unlock()
		if err != nil {
			c.metrics.setReason("conversion-failure")
			if inbound != nil {
				_ = inbound.Close()
			}
			c.finishEpoch(ctx, bus, sessionID, "outbound-start")
			return err
		}
	} else {
		c.setOutbound(sessionID, nil)
	}
	c.setHealth(CrosstalkHealth{
		Ready:     true,
		Reason:    "ready",
		SessionID: sessionID,
		Assigned:  welcome.AssignedSessionID,
		Peer:      welcome.PeerID,
		Epoch:     welcome.Epoch,
	})

	var tracks atomic.Uint32
	session.OnTrack(func(track ABCTrack) {
		if tracks.Add(1) != 1 {
			c.metrics.Dropped.Add(1)
			c.logger.Info("crosstalk ignored extra monitor track", "stage", c.cfg.Name, "qol_session", sessionID)
			return
		}
		if inbound == nil {
			return
		}
		if err := media.ValidateCodec(track.Codec()); err != nil {
			select {
			case errc <- err:
			default:
			}
			return
		}
		go c.readTrack(epochCtx, track, inbound)
	})
	session.OnClose(func(reason string) {
		c.metrics.setReason(reason)
		cancel()
	})

	var runErr error
	select {
	case <-ctx.Done():
		c.metrics.setReason("canceled")
	case <-session.Done():
		if runErr = session.Err(); runErr != nil {
			c.metrics.SourceErrors.Add(1)
		}
		if c.metrics.Reason() == "" {
			c.metrics.setReason("closed")
		}
	case err := <-errc:
		runErr = err
		c.metrics.setReason("conversion-failure")
		c.logger.Error("crosstalk converter failed", "stage", c.cfg.Name, "qol_session", sessionID, "epoch", welcome.Epoch, "err", err)
	}
	cancel()
	if inbound != nil {
		_ = inbound.WriteFrame(media.Frame{End: true})
		_ = inbound.Close()
	}
	workers.Wait()
	c.finishEpoch(ctx, bus, sessionID, c.metrics.Reason())
	return runErr
}

func converterError(direction string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("crosstalk %s conversion failed: %w", direction, err)
}

func (c *Crosstalk) failEpoch(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	errc := c.epochErrc
	c.mu.Unlock()
	if errc == nil {
		return
	}
	select {
	case errc <- err:
	default:
	}
}

func (c *Crosstalk) setHealth(health CrosstalkHealth) {
	c.mu.Lock()
	c.health = health
	c.mu.Unlock()
}

func (c *Crosstalk) waitBackoff(ctx context.Context, backoff *time.Duration) bool {
	delay := nextBackoff(c.cfg, *backoff)
	*backoff = delay
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

func (c *Crosstalk) setOutbound(sessionID string, outbound *media.Outbound) {
	c.mu.Lock()
	prev := c.outbound
	c.outbound = outbound
	c.epochSessionID = sessionID
	c.epochReady = true
	c.activeSession = ""
	c.nextSeq = 0
	c.sourceSeq = 0
	c.eosSent = false
	c.sinkSeq = 0
	c.mu.Unlock()
	if prev != nil {
		_ = prev.Close()
	}
}

func (c *Crosstalk) finishEpoch(ctx context.Context, bus qol.Bus, sessionID, reason string) {
	c.mu.Lock()
	outbound := c.outbound
	c.outbound = nil
	c.epochCtx = nil
	c.epochABC = nil
	ready := c.epochReady
	c.epochReady = false
	c.activeSession = ""
	c.nextSeq = 0
	c.mu.Unlock()
	if outbound != nil {
		_ = outbound.Close()
	}
	if !ready || c.cfg.OutputChannel == "" || sessionID == "" {
		return
	}
	_ = c.publishEOS(ctx, bus, sessionID, reason)
}

// startOutboundLocked starts a fresh converter for the next sink stream. c.mu
// must be held; the caller publishes the converter only after Start succeeds.
func (c *Crosstalk) startOutboundLocked() error {
	if c.epochCtx == nil || c.epochABC == nil {
		return errors.New("crosstalk sink epoch is not active")
	}
	outbound, err := media.NewOutbound(media.OutboundConfig{
		FFmpeg: c.cfg.FFmpeg, Codec: c.epochABCCodec(), Queue: c.cfg.Queue, Overflow: c.cfg.Overflow,
	})
	if err != nil {
		return err
	}
	if err := outbound.Start(c.epochCtx); err != nil {
		_ = outbound.Close()
		return err
	}
	epochCtx := c.epochCtx
	session := c.epochABC
	c.outbound = outbound
	go func() {
		if err := c.forwardOutbound(epochCtx, session, outbound); err != nil && epochCtx.Err() == nil {
			c.failEpoch(err)
		}
	}()
	return nil
}

func (c *Crosstalk) epochABCCodec() media.Codec {
	codec, ok := c.epochABC.NegotiatedCodec()
	if !ok {
		return media.Codec{MimeType: media.MimeTypeOpus, ClockRate: media.OpusClockRate, Channels: 1}
	}
	return codec
}

// finishSinkSessionLocked waits for the current stream's RTP to drain, then
// atomically replaces it with a fresh converter. c.mu must be held.
func (c *Crosstalk) finishSinkSessionLocked(outbound *media.Outbound) error {
	err := outbound.Wait()
	if c.outbound != outbound {
		return nil
	}
	c.outbound = nil
	c.activeSession = ""
	c.nextSeq = 0
	if err != nil && c.epochCtx.Err() == nil {
		return err
	}
	if c.epochCtx.Err() != nil {
		return nil
	}
	return c.startOutboundLocked()
}

func (c *Crosstalk) handleSink(_ context.Context, event qol.Event) error {
	switch event.Type {
	case qol.TypeAudioStream, qol.TypeAudioPCM, qol.TypeAudioPCMFinal:
	default:
		c.metrics.Dropped.Add(1)
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.epochReady || c.outbound == nil {
		c.metrics.Dropped.Add(1)
		return nil
	}
	if c.activeSession == "" {
		if event.Seq != 0 {
			c.metrics.Dropped.Add(1)
			return nil
		}
		c.activeSession = event.SessionID
		c.nextSeq = 0
	}
	if event.SessionID != c.activeSession {
		c.metrics.RejectedSessions.Add(1)
		c.logger.Info("crosstalk rejected extra qol session", "stage", c.cfg.Name, "direction", "sink", "session", event.SessionID, "active", c.activeSession)
		return nil
	}
	if event.Seq < c.nextSeq {
		c.metrics.Duplicates.Add(1)
		return nil
	}
	if event.Seq != c.nextSeq {
		c.metrics.Dropped.Add(1)
		c.logger.Info("crosstalk dropped non-contiguous sink event", "stage", c.cfg.Name, "expected", c.nextSeq, "got", event.Seq)
		return nil
	}
	end := event.Type == qol.TypeAudioPCMFinal
	if event.Type == qol.TypeAudioStream {
		_, _, streamEnd, _, err := wire.UnmarshalAudioStream(event.Payload)
		if err == nil {
			end = streamEnd
		}
	}
	if err := c.outbound.WriteEvent(event); err != nil {
		if errors.Is(err, media.ErrOverflow) {
			dropped := c.metrics.Dropped.Add(1)
			c.logger.Info("crosstalk overflow", "stage", c.cfg.Name, "direction", "sink", "dropped", dropped, "qol_session", c.health.SessionID)
			return nil
		}
		c.metrics.SinkErrors.Add(1)
		c.logger.Error("crosstalk sink write failed", "stage", c.cfg.Name, "err", err)
		return err
	}
	c.nextSeq++
	if !end {
		return nil
	}
	if err := c.finishSinkSessionLocked(c.outbound); err != nil {
		c.metrics.SinkErrors.Add(1)
		c.logger.Error("crosstalk sink session ended with error", "stage", c.cfg.Name, "err", err)
		return err
	}
	return nil
}

func (c *Crosstalk) forwardOutbound(ctx context.Context, session ABCSession, outbound *media.Outbound) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-outbound.Frames():
			if !ok {
				if err := outbound.Err(); err != nil && ctx.Err() == nil {
					c.metrics.SinkErrors.Add(1)
					return converterError("sink", err)
				}
				return nil
			}
			if frame.End {
				return nil
			}
			if len(frame.Payload) == 0 {
				continue
			}
			packet := media.PackRTP(frame)
			c.sinkRTMu.Lock()
			packet.SequenceNumber = c.sinkSeq
			c.sinkSeq++
			c.sinkRTMu.Unlock()
			if err := session.WriteRTP(packet); err != nil {
				c.metrics.SinkErrors.Add(1)
				return fmt.Errorf("crosstalk write rtp: %w", err)
			}
			c.metrics.SinkFrames.Add(1)
		}
	}
}

func (c *Crosstalk) readTrack(ctx context.Context, track ABCTrack, inbound *media.Inbound) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		packet, err := track.ReadRTP()
		if err != nil {
			if isTemporaryNetErr(err) {
				continue
			}
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				c.metrics.SourceErrors.Add(1)
			}
			return
		}
		frame := media.Frame{
			Payload:     append([]byte(nil), packet.Payload...),
			Sequence:    packet.SequenceNumber,
			Timestamp:   packet.Timestamp,
			Marker:      packet.Marker,
			PayloadType: packet.PayloadType,
			SSRC:        packet.SSRC,
		}
		if err := inbound.WriteFrame(frame); err != nil {
			if errors.Is(err, media.ErrOverflow) {
				dropped := c.metrics.Dropped.Add(1)
				c.logger.Info("crosstalk overflow", "stage", c.cfg.Name, "direction", "source", "dropped", dropped, "qol_session", c.Health().SessionID)
				continue
			}
			if !errors.Is(err, media.ErrClosed) {
				c.metrics.SourceErrors.Add(1)
			}
			return
		}
		c.metrics.SourceFrames.Add(1)
	}
}

func (c *Crosstalk) publishInbound(ctx context.Context, bus qol.Bus, sessionID string, inbound *media.Inbound) error {
	var last qol.MediaTime
	for {
		select {
		case <-ctx.Done():
			return nil
		case chunk, ok := <-inbound.Chunks():
			if !ok {
				if err := inbound.Err(); err != nil && ctx.Err() == nil {
					return converterError("source", err)
				}
				return nil
			}
			if chunk.End && len(chunk.Data) == 0 {
				return c.publishEOS(ctx, bus, sessionID, "inbound-end")
			}
			if err := c.publishChunk(ctx, bus, sessionID, chunk, last); err != nil {
				return err
			}
			if chunk.Span.End > last {
				last = chunk.Span.End
			}
			if chunk.End {
				return nil
			}
		}
	}
}

func (c *Crosstalk) nextSourceSeq(end bool) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.eosSent {
		return 0, false
	}
	seq := c.sourceSeq
	c.sourceSeq++
	if end {
		c.eosSent = true
	}
	return seq, true
}

func (c *Crosstalk) publishChunk(ctx context.Context, bus qol.Bus, sessionID string, chunk media.OutputChunk, last qol.MediaTime) error {
	span := chunk.Span
	if span.End < span.Start {
		return fmt.Errorf("crosstalk non-monotonic span")
	}
	if last > 0 && span.Start < last && len(chunk.Data) > 0 {
		return fmt.Errorf("crosstalk non-monotonic span")
	}
	seq, ok := c.nextSourceSeq(chunk.End)
	if !ok {
		return nil
	}
	payload, eventType, err := encodeSourceChunk(c.cfg.OutputProfile, chunk)
	if err != nil {
		return err
	}
	event := qol.Event{
		ID:         eventID(c.cfg.Name, sessionID, seq),
		SessionID:  sessionID,
		Channel:    c.cfg.OutputChannel,
		Type:       eventType,
		Seq:        seq,
		Span:       span,
		ProducedAt: c.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	}
	if err := bus.Publish(ctx, event); err != nil {
		c.metrics.SourceErrors.Add(1)
		return err
	}
	return nil
}

func (c *Crosstalk) publishEOS(ctx context.Context, bus qol.Bus, sessionID, reason string) error {
	publishCtx := ctx
	if ctx.Err() != nil {
		publishCtx = context.Background()
	}
	seq, ok := c.nextSourceSeq(true)
	if !ok {
		return nil
	}
	payload, eventType, err := encodeSourceChunk(c.cfg.OutputProfile, media.OutputChunk{End: true})
	if err != nil {
		return err
	}
	event := qol.Event{
		ID:         eventID(c.cfg.Name, sessionID, seq),
		SessionID:  sessionID,
		Channel:    c.cfg.OutputChannel,
		Type:       eventType,
		Seq:        seq,
		ProducedAt: c.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	}
	c.logger.Info("crosstalk epoch eos", "stage", c.cfg.Name, "qol_session", sessionID, "reason", reason)
	return bus.Publish(publishCtx, event)
}

func encodeSourceChunk(profile media.Profile, chunk media.OutputChunk) ([]byte, string, error) {
	if profile.Kind == media.ProfileOggOpus {
		data := chunk.Data
		if len(data) == 0 {
			data = []byte{0}
		}
		payload, err := wire.MarshalAudioStream(media.MediaTypeOggOpus, "crosstalk", chunk.End, data)
		return payload, qol.TypeAudioStream, err
	}
	data := chunk.Data
	if len(data) == 0 {
		data = []byte{0, 0}
	}
	payload, err := wire.MarshalAudio(qol.Audio{Format: profile.PCMFormat(), Data: data})
	return payload, profile.EventType(chunk.End), err
}

func isTemporaryNetErr(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

var _ qol.Stage = (*Crosstalk)(nil)
