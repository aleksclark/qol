package eventbus_test

import (
	"testing"

	"github.com/aleksclark/qol/internal/eventbus"
)

func TestConnectRejectsIncompleteConfiguration(t *testing.T) {
	for name, options := range map[string]eventbus.ConnectOptions{
		"no url":           {URLs: " "},
		"user only":        {URLs: "nats://127.0.0.1:1", User: "qol"},
		"password only":    {URLs: "nats://127.0.0.1:1", Password: "secret"},
		"unreachable":      {URLs: "nats://127.0.0.1:1"},
		"unreachable auth": {URLs: "nats://127.0.0.1:1,nats://127.0.0.1:2", User: "qol", Password: "secret"},
	} {
		if connection, err := eventbus.Connect(options); err == nil {
			connection.Close()
			t.Fatalf("%s: expected an error", name)
		}
	}
}
