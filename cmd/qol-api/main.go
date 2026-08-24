package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aleksclark/qol/internal/api"
	"github.com/aleksclark/qol/internal/auth"
	"github.com/aleksclark/qol/internal/config"
	"github.com/aleksclark/qol/internal/eventbus"
	"github.com/aleksclark/qol/internal/store/disk"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

func main() {
	if err := newCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	root := &cobra.Command{Use: "qol-api", Short: "Qol control plane"}
	root.PersistentFlags().String("nats-url", nats.DefaultURL, "NATS server URL")
	root.PersistentFlags().String("data-dir", "/var/lib/qol", "Local persistence directory")
	serve := &cobra.Command{Use: "serve", Short: "Run the API", RunE: runServe}
	serve.Flags().String("http-address", ":8080", "HTTP API address")
	serve.Flags().String("webtransport-address", ":4443", "HTTP/3 WebTransport address")
	serve.Flags().String("tls-cert", "", "TLS certificate path")
	serve.Flags().String("tls-key", "", "TLS private key path")
	serve.Flags().String("allowed-origin", "http://localhost:5173", "Allowed browser origin")
	serve.Flags().Duration("session-ttl", 24*time.Hour, "Login session lifetime")
	serve.Flags().Duration("ticket-ttl", time.Minute, "Upload ticket lifetime")
	bootstrap := &cobra.Command{Use: "bootstrap", Short: "Create the initial administrator", RunE: runBootstrap}
	bootstrap.Flags().String("username", "", "Administrator username")
	bootstrap.Flags().String("password", "", "Administrator password")
	admin := &cobra.Command{Use: "admin", Short: "Administrative commands"}
	admin.AddCommand(bootstrap)
	root.AddCommand(serve, admin)
	return root
}

func runServe(command *cobra.Command, _ []string) error {
	settings, err := config.Bind(command)
	if err != nil {
		return err
	}
	connection, _, authentication, bus, err := dependencies(settings.GetString("nats-url"), settings.GetString("data-dir"), settings.GetDuration("session-ttl"), settings.GetDuration("ticket-ttl"))
	if err != nil {
		return err
	}
	defer connection.Close()
	server := api.New(authentication, bus, settings.GetString("allowed-origin"), settings.GetString("tls-cert") != "")
	ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	httpServer := &http.Server{Addr: settings.GetString("http-address"), Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- httpServer.ListenAndServe() }()
	if settings.GetString("tls-cert") != "" {
		transport := server.Transport()
		transport.H3.Addr = settings.GetString("webtransport-address")
		go func() {
			errorsChannel <- transport.ListenAndServeTLS(settings.GetString("tls-cert"), settings.GetString("tls-key"))
		}()
	}
	slog.Info("Qol API running", "http", settings.GetString("http-address"), "webtransport", settings.GetString("webtransport-address"))
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
		return nil
	case err := <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func runBootstrap(command *cobra.Command, _ []string) error {
	settings, err := config.Bind(command)
	if err != nil {
		return err
	}
	username, password := settings.GetString("username"), settings.GetString("password")
	if username == "" || password == "" {
		return errors.New("username and password are required through flags or QOL_USERNAME and QOL_PASSWORD")
	}
	connection, _, authentication, _, err := dependencies(settings.GetString("nats-url"), settings.GetString("data-dir"), 24*time.Hour, time.Minute)
	if err != nil {
		return err
	}
	defer connection.Close()
	if err := authentication.Bootstrap(command.Context(), username, password); err != nil {
		return fmt.Errorf("bootstrap administrator: %w", err)
	}
	fmt.Fprintln(command.OutOrStdout(), "Administrator created")
	return nil
}

func dependencies(url, dataDir string, sessionTTL, ticketTTL time.Duration) (*nats.Conn, *disk.Store, *auth.Service, *eventbus.Bus, error) {
	connection, err := nats.Connect(url)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if dataDir == "" {
		dataDir = "/var/lib/qol"
	}
	storage, err := disk.New(dataDir)
	if err != nil {
		connection.Close()
		return nil, nil, nil, nil, err
	}
	for bucket, ttl := range map[string]time.Duration{auth.UsersBucket: 0, auth.SessionsBucket: sessionTTL, auth.TicketsBucket: ticketTTL} {
		if err := storage.EnsureBucket(bucket, ttl); err != nil {
			connection.Close()
			return nil, nil, nil, nil, err
		}
	}
	bus, err := eventbus.New(connection)
	if err != nil {
		connection.Close()
		return nil, nil, nil, nil, err
	}
	if err := bus.WatchActivity(); err != nil {
		connection.Close()
		return nil, nil, nil, nil, err
	}
	return connection, storage, auth.New(storage, sessionTTL, ticketTTL), bus, nil
}
