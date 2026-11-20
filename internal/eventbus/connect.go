package eventbus

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

type ConnectOptions struct {
	URLs     string
	User     string
	Password string
	Name     string
}

func Connect(options ConnectOptions) (*nats.Conn, error) {
	if strings.TrimSpace(options.URLs) == "" {
		return nil, errors.New("at least one NATS URL is required")
	}
	if (options.User == "") != (options.Password == "") {
		return nil, errors.New("NATS user and password must be set together")
	}
	natsOptions := []nats.Option{
		nats.Name(options.Name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.RetryOnFailedConnect(false),
		nats.DisconnectErrHandler(func(connection *nats.Conn, err error) {
			slog.Warn("NATS disconnected", "error", err)
		}),
		nats.ReconnectHandler(func(connection *nats.Conn) {
			slog.Info("NATS reconnected", "server", connection.ConnectedServerName())
		}),
		nats.ErrorHandler(func(_ *nats.Conn, subscription *nats.Subscription, err error) {
			subject := ""
			if subscription != nil {
				subject = subscription.Subject
			}
			slog.Error("NATS asynchronous error", "subject", subject, "error", err)
		}),
	}
	if options.User != "" {
		natsOptions = append(natsOptions, nats.UserInfo(options.User, options.Password))
	}
	return nats.Connect(options.URLs, natsOptions...)
}
