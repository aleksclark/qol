package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aleksclark/qol/internal/config"
	"github.com/aleksclark/qol/internal/eventbus"
	"github.com/aleksclark/qol/internal/worker"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

func main() {
	if err := newCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	command := &cobra.Command{Use: "qol-worker", Short: "Qol event worker"}
	command.PersistentFlags().String("nats-url", nats.DefaultURL, "NATS server URL")
	command.AddCommand(&cobra.Command{Use: "run", Short: "Run event handlers", RunE: run})
	return command
}

func run(command *cobra.Command, _ []string) error {
	settings, err := config.Bind(command)
	if err != nil {
		return err
	}
	connection, err := nats.Connect(settings.GetString("nats-url"))
	if err != nil {
		return err
	}
	defer connection.Close()
	bus, err := eventbus.New(connection)
	if err != nil {
		return err
	}
	echo := worker.NewEcho(bus)
	subscription, err := bus.SubscribeInputs("qol-echo-v1", echo.Handle)
	if err != nil {
		return err
	}
	defer subscription.Unsubscribe()
	ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.Info("Qol worker running")
	<-ctx.Done()
	return nil
}
