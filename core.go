package qol

import (
	"context"
	"time"
)

// MediaTime is an offset from the beginning of an AgentRun/session media clock.
//
// Nanoseconds are convenient because they map directly to time.Duration while
// remaining language-neutral on the wire.
type MediaTime int64

func (t MediaTime) Duration() time.Duration {
	return time.Duration(t)
}

// Span identifies the portion of the source media timeline represented by an
// event.
//
// Instantaneous events may use Start == End.
type Span struct {
	Start MediaTime
	End   MediaTime
}

// Event is the unit exchanged between stages.
//
// Seq provides ordering within a particular producer/channel.
// Span provides alignment against the shared media timeline.
// ProducedAt is wall-clock metadata for observability, not media alignment.
type Event struct {
	ID        string
	SessionID string

	// Logical channel, e.g.
	//   audio
	//   asr.text
	//   asr.prosody
	//   translation.text
	//   tts.audio
	Channel string

	// Semantic payload type, e.g.
	//   qol.audio.pcm
	//   qol.text.partial
	//   qol.text.final
	//   qol.prosody
	Type string

	Seq  uint64
	Span Span

	// IDs of events from which this event was derived.
	Parents []string

	ProducedAt time.Time

	// Encoding identifies Payload's wire representation.
	//
	// Examples:
	//   application/protobuf
	//   application/json
	//   audio/pcm;rate=16000;channels=1;format=s16le
	Encoding string

	Payload []byte
}

// Publisher emits events onto a logical channel.
//
// A Publisher may be backed by NATS, an in-process bus, a test implementation,
// or some other transport.
type Publisher interface {
	Publish(ctx context.Context, event Event) error
}

// Handler consumes events from a subscription.
//
// Returning an error indicates that processing failed. Durable transports may
// use this result to decide whether to acknowledge or redeliver an event.
type Handler func(context.Context, Event) error

// Subscription represents an active event stream subscription.
type Subscription interface {
	// Close stops delivery and releases resources.
	Close() error

	// Done closes when the subscription terminates.
	Done() <-chan struct{}

	// Err returns the terminal subscription error, if any.
	Err() error
}

// SubscribeOptions describe delivery semantics without tying the API directly
// to a particular broker.
type SubscribeOptions struct {
	// Durable requests retained/replayable delivery where supported.
	Durable bool

	// Consumer identifies a durable consumer across restarts.
	// Ignored when Durable is false.
	Consumer string

	// Group enables competing-consumer semantics.
	//
	// Subscribers with the same group divide messages between themselves.
	// Subscribers without a group each receive their own copy.
	Group string

	// Replay controls where a subscription begins.
	Replay ReplayPosition
}

type ReplayPolicy uint8

const (
	ReplayLive ReplayPolicy = iota
	ReplayAll
	ReplayFromSequence
	ReplayFromTime
)

type ReplayPosition struct {
	Policy   ReplayPolicy
	Sequence uint64
	Time     time.Time
}

// Subscriber receives events matching a channel pattern.
//
// Implementations may support transport-specific wildcards such as:
//
//	session.*.asr.text
//
// The exact naming convention is intentionally outside the stage interfaces.
type Subscriber interface {
	Subscribe(
		ctx context.Context,
		channel string,
		opts SubscribeOptions,
		handler Handler,
	) (Subscription, error)
}

// Bus is the complete event transport required by most stages.
type Bus interface {
	Publisher
	Subscriber
}

// Port describes one logical input or output of a stage.
//
// Type is documentation/validation metadata; transports do not need to
// interpret it.
type Port struct {
	Name string

	// Channel is the channel or channel pattern used by this port.
	Channel string

	// Accepted or produced event types.
	Types []string
}

// StageSpec describes a stage and its externally visible streams.
//
// This can be used for introspection, validation, graph construction,
// visualization, and automatic wiring.
type StageSpec struct {
	Name string

	Inputs  []Port
	Outputs []Port
}

// Stage is one continuously-running processing component.
//
// A stage typically subscribes to one or more input channels and publishes to
// one or more output channels until ctx is cancelled.
type Stage interface {
	Spec() StageSpec

	Run(ctx context.Context, bus Bus) error
}
