package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"strconv"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

type LiveProcessor interface {
	Start(context.Context) (io.WriteCloser, io.ReadCloser, func() error, error)
}

type StreamAudioStore interface {
	Open(context.Context, string) (StreamAudioWriter, error)
}

type StreamAudioWriter interface {
	Write([]byte) error
	Close() (qol.StoredAudio, error)
	Abort() error
}

type chunk struct {
	data []byte
	end  bool
}

type streamState struct {
	next    uint64
	outSeq  uint64
	parents []string
	span    qol.Span
	writer  StreamAudioWriter
	input   io.WriteCloser
	output  io.ReadCloser
	wait    func() error
	done    chan error
	cancel  context.CancelFunc
}

func runSubscription(ctx context.Context, bus qol.Bus, channel, group string, handler qol.Handler) error {
	subscription, err := bus.Subscribe(ctx, channel, qol.SubscribeOptions{Group: group}, handler)
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

func decodeAudioStream(event qol.Event) (chunk, error) {
	if event.Type != qol.TypeAudioStream {
		return chunk{}, errors.New("stage requires an audio stream event")
	}
	_, _, end, data, err := wire.UnmarshalAudioStream(event.Payload)
	return chunk{data: data, end: end}, err
}

func streamKey(event qol.Event) string {
	return event.SessionID + "\x00" + event.Channel
}

func SafeSessionName(sessionID string) string {
	return filepath.Base(sessionID)
}

func eventID(stage, sessionID string, sequence uint64) string {
	hash := sha256.New()
	for _, part := range []string{stage, sessionID, strconv.FormatUint(sequence, 10)} {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
