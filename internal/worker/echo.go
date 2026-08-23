package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/aleksclark/qol/domain"
)

type Echo struct {
	publisher domain.Publisher
}

func NewEcho(publisher domain.Publisher) *Echo {
	return &Echo{publisher: publisher}
}

func (e *Echo) Handle(ctx context.Context, input domain.Event) error {
	output := input
	output.ID = eventID("echo-v1", input.ID)
	output.Parents = []string{input.ID}
	output.Produced = time.Now().UTC()
	return e.publisher.Publish(ctx, fmt.Sprintf("qol.session.%s.pcm.output", input.SessionID), output)
}

func eventID(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
