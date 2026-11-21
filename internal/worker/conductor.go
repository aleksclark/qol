package worker

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

const (
	DefaultCondenseLag    = 6 * time.Second
	DefaultCondenseRatio  = 0.65
	DefaultSummarizeTurns = 16
	conductorCooldown     = 1500 * time.Millisecond
	conductorRetention    = 10 * time.Minute
	conductorProtected    = 2
)

type ConductorOptions struct {
	CondenseLag    time.Duration
	CondenseRatio  float32
	SummarizeTurns uint32
}

func DefaultConductorOptions() ConductorOptions {
	return ConductorOptions{CondenseLag: DefaultCondenseLag, CondenseRatio: DefaultCondenseRatio, SummarizeTurns: DefaultSummarizeTurns}
}

type conductorSession struct {
	controlSeq       uint64
	requested        map[uint64]bool
	lastCondense     time.Time
	summarizePending bool
	seen             time.Time
}

type Conductor struct {
	spec      qol.StageSpec
	options   ConductorOptions
	now       func() time.Time
	publisher qol.Publisher
	ctx       context.Context
	mu        sync.Mutex
	sessions  map[string]*conductorSession
}

func NewConductor(options ConductorOptions) *Conductor {
	defaults := DefaultConductorOptions()
	if options.CondenseLag <= 0 {
		options.CondenseLag = defaults.CondenseLag
	}
	if options.CondenseRatio <= 0 || options.CondenseRatio >= 1 {
		options.CondenseRatio = defaults.CondenseRatio
	}
	if options.SummarizeTurns == 0 {
		options.SummarizeTurns = defaults.SummarizeTurns
	}
	return &Conductor{
		spec: qol.StageSpec{
			Name:    "conductor",
			Inputs:  []qol.Port{{Name: "telemetry", Channel: qol.ChannelTelemetry, Types: []string{qol.TypeTelemetry}}},
			Outputs: []qol.Port{{Name: "translate-control", Channel: qol.ChannelTranslateControl, Types: []string{qol.TypeControl}}},
		},
		options: options, now: time.Now, sessions: make(map[string]*conductorSession),
	}
}

func (c *Conductor) Spec() qol.StageSpec {
	return c.spec
}

func (c *Conductor) Run(ctx context.Context, bus qol.Bus) error {
	c.ctx, c.publisher = ctx, bus
	return runSubscription(ctx, bus, qol.ChannelTelemetry, "qol-conductor", c.handle)
}

func (c *Conductor) handle(_ context.Context, event qol.Event) error {
	if event.Type != qol.TypeTelemetry {
		return nil
	}
	telemetry, err := wire.UnmarshalTelemetry(event.Payload)
	if err != nil {
		return err
	}
	var controls []qol.Control
	c.mu.Lock()
	now := c.now()
	session := c.sessions[event.SessionID]
	if session == nil {
		session = &conductorSession{requested: make(map[uint64]bool), seen: now}
		c.sessions[event.SessionID] = session
		for id, other := range c.sessions {
			if now.Sub(other.seen) > conductorRetention {
				delete(c.sessions, id)
			}
		}
	}
	session.seen = now
	switch telemetry.Stage {
	case "tts":
		if control, ok := c.condenseLocked(session, telemetry, now); ok {
			controls = append(controls, control)
		}
	case "translate":
		if telemetry.ContextTurns >= c.options.SummarizeTurns && !session.summarizePending {
			session.summarizePending = true
			controls = append(controls, qol.Control{Kind: qol.ControlSummarize})
		} else if telemetry.ContextTurns < c.options.SummarizeTurns {
			session.summarizePending = false
		}
	}
	sequences := make([]uint64, len(controls))
	for index := range controls {
		sequences[index] = session.controlSeq
		session.controlSeq++
	}
	c.mu.Unlock()
	for index, control := range controls {
		if err := c.send(event, sequences[index], control); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conductor) condenseLocked(session *conductorSession, telemetry qol.Telemetry, now time.Time) (qol.Control, bool) {
	if telemetry.ProjectedLagSeconds <= c.options.CondenseLag.Seconds() || len(telemetry.QueuedSequences) <= conductorProtected || now.Sub(session.lastCondense) < conductorCooldown {
		return qol.Control{}, false
	}
	var targets []uint64
	for _, sequence := range telemetry.QueuedSequences[conductorProtected:] {
		if !session.requested[sequence] {
			targets = append(targets, sequence)
		}
	}
	if len(targets) == 0 {
		return qol.Control{}, false
	}
	slices.Sort(targets)
	for _, sequence := range targets {
		session.requested[sequence] = true
	}
	session.lastCondense = now
	slog.Info("conductor requested condensing", "projected_lag", telemetry.ProjectedLagSeconds, "targets", targets)
	return qol.Control{Kind: qol.ControlCondense, TargetSequences: targets, TargetRatio: c.options.CondenseRatio}, true
}

func (c *Conductor) send(source qol.Event, sequence uint64, control qol.Control) error {
	payload, err := wire.MarshalControl(control)
	if err != nil {
		return err
	}
	return c.publisher.Publish(c.ctx, qol.Event{
		ID:         eventID(c.spec.Name, source.SessionID, sequence),
		SessionID:  source.SessionID,
		Channel:    qol.ChannelTranslateControl,
		Type:       qol.TypeControl,
		Seq:        sequence,
		Span:       source.Span,
		Parents:    []string{source.ID},
		ProducedAt: c.now().UTC(),
		Encoding:   qol.EncodingProtobuf,
		Payload:    payload,
	})
}

var _ qol.Stage = (*Conductor)(nil)
