package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/config"
	"github.com/aleksclark/qol/internal/eventbus"
	"github.com/aleksclark/qol/internal/media"
	"github.com/aleksclark/qol/internal/worker"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const defaultHealthFile = "/tmp/qol-crosstalk-health"

func main() {
	if err := newCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	command := &cobra.Command{Use: "qol-worker", Short: "Qol event worker"}
	command.PersistentFlags().String("nats-url", nats.DefaultURL, "NATS server URL")
	command.PersistentFlags().String("health-file", defaultHealthFile, "Structured health file written by crosstalk")
	runCommand := &cobra.Command{Use: "run", Short: "Run one streaming graph stage", RunE: run}
	runCommand.Flags().String("type", "", "Stage type: capture, stt-whisper, translate, tts, record, or crosstalk")
	runCommand.Flags().String("stage", "", "Deprecated alias for --type")
	runCommand.Flags().String("name", "", "Stage instance name; defaults to the type")
	runCommand.Flags().String("input", "", "Input channel override for record")
	runCommand.Flags().String("output", "", "Output channel override for record")
	runCommand.Flags().String("processor", "", "Streaming processor executable")
	runCommand.Flags().StringSlice("processor-arg", nil, "Streaming processor argument")
	runCommand.Flags().String("spool-dir", "/tmp/qol", "Graph spool directory")
	runCommand.Flags().String("output-dir", "/output", "Ogg/Opus output directory")
	runCommand.Flags().String("ffmpeg", "ffmpeg", "ffmpeg executable")
	runCommand.Flags().String("crosstalk-url", "", "Crosstalk signaling URL")
	runCommand.Flags().String("crosstalk-token", "", "ABC token; prefer --crosstalk-token-file")
	runCommand.Flags().String("crosstalk-token-file", "", "File containing the ABC token")
	runCommand.Flags().String("crosstalk-source-channel", "", "Qol output channel for Crosstalk monitor audio")
	runCommand.Flags().String("crosstalk-sink-channel", "", "Qol input channel published to Crosstalk")
	runCommand.Flags().String("crosstalk-output-profile", "pcm-s16le", "Source output profile: pcm-s16le, pcm-f32le, or ogg-opus")
	runCommand.Flags().Int("crosstalk-output-rate", 16000, "PCM sample rate for Crosstalk source output")
	runCommand.Flags().Int("crosstalk-output-channels", 1, "PCM channels for Crosstalk source output")
	runCommand.Flags().Int("crosstalk-queue", 64, "Bounded converter queue size")
	runCommand.Flags().String("crosstalk-overflow", "drop", "Overflow policy: drop or fail")
	runCommand.Flags().Bool("crosstalk-reconnect", true, "Reconnect after transient ABC failures")
	runCommand.Flags().Duration("crosstalk-min-backoff", 250*time.Millisecond, "Minimum reconnect backoff")
	runCommand.Flags().Duration("crosstalk-max-backoff", 8*time.Second, "Maximum reconnect backoff")
	runCommand.Flags().String("crosstalk-group", "", "Competing-consumer group; unused unless --crosstalk-competing")
	runCommand.Flags().Bool("crosstalk-competing", false, "Subscribe as a competing consumer instead of fan-out")
	runCommand.Flags().String("crosstalk-client-name", "qol", "ABC client name sent during Hello")
	runCommand.Flags().Bool("crosstalk-disable-mdns", false, "Disable ICE mDNS gathering (localhost)")
	runCommand.Flags().Bool("crosstalk-disable-stun", false, "Disable STUN so only host ICE candidates are used")
	healthCommand := &cobra.Command{Use: "health", Short: "Exit 0 only when the worker health file is ready", RunE: health}
	command.AddCommand(runCommand, healthCommand)
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
	stageType := settings.GetString("type")
	if stageType == "" {
		stageType = settings.GetString("stage")
	}
	stage, err := buildStage(stageType, settings)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if crosstalk, ok := stage.(*worker.Crosstalk); ok {
		go watchHealth(ctx, settings.GetString("health-file"), crosstalk)
	}
	slog.Info("Qol worker running", "type", stageType, "name", stage.Spec().Name)
	return stage.Run(ctx, bus)
}

func health(command *cobra.Command, _ []string) error {
	settings, err := config.Bind(command)
	if err != nil {
		return err
	}
	path := settings.GetString("health-file")
	if path == "" {
		path = defaultHealthFile
	}
	payload, err := readHealthFile(path)
	if err != nil {
		return err
	}
	if !payload.Ready {
		return fmt.Errorf("worker not ready: %s", payload.Reason)
	}
	return nil
}

func buildStage(stageType string, settings *viper.Viper) (qol.Stage, error) {
	name := settings.GetString("name")
	if name == "" {
		name = stageType
	}
	input := settings.GetString("input")
	output := settings.GetString("output")
	processor := settings.GetString("processor")
	args := settings.GetStringSlice("processor-arg")
	spoolDir := settings.GetString("spool-dir")
	outputDir := settings.GetString("output-dir")
	ffmpeg := settings.GetString("ffmpeg")
	switch stageType {
	case "capture":
		return worker.NewCapture(), nil
	case "stt-whisper":
		if processor == "" {
			return nil, errors.New("stt-whisper requires --processor or QOL_PROCESSOR")
		}
		return worker.NewSTT(worker.NewCommandProcessor(processor, args...), spoolDir), nil
	case "translate":
		if processor == "" {
			return nil, errors.New("translate requires --processor or QOL_PROCESSOR")
		}
		return worker.NewTranslate(worker.NewCommandProcessor(processor, args...), spoolDir), nil
	case "tts":
		if processor == "" {
			return nil, errors.New("tts requires --processor or QOL_PROCESSOR")
		}
		return worker.NewTTS(worker.NewCommandProcessor(processor, args...), spoolDir), nil
	case "record":
		if input == "" || output == "" {
			return nil, fmt.Errorf("record instance %s requires --input and --output", name)
		}
		store := worker.NewFFmpegOggOpusStore(ffmpeg, outputDir)
		if input == qol.ChannelTTSAudio {
			store = worker.NewFFmpegS16LEOggOpusStore(ffmpeg, outputDir, 22050, 1)
		}
		return worker.NewRecord(name, input, output, store, spoolDir), nil
	case "crosstalk":
		cfg, err := buildCrosstalkConfig(settings, name, ffmpeg)
		if err != nil {
			return nil, err
		}
		return worker.NewCrosstalk(cfg)
	default:
		return nil, errors.New("stage type must be capture, stt-whisper, translate, tts, record, or crosstalk")
	}
}

func buildCrosstalkConfig(settings *viper.Viper, name, ffmpeg string) (worker.CrosstalkConfig, error) {
	tokenFile := settings.GetString("crosstalk-token-file")
	token, err := config.ReadSecret(settings.GetString("crosstalk-token"), tokenFile)
	if err != nil {
		return worker.CrosstalkConfig{}, err
	}
	if token == "" {
		return worker.CrosstalkConfig{}, errors.New("crosstalk requires --crosstalk-token-file or --crosstalk-token")
	}
	if tokenFile == "" {
		slog.Warn("crosstalk token loaded from environment or flag; prefer --crosstalk-token-file")
	}
	profile, err := media.ParseProfile(settings.GetString("crosstalk-output-profile"), settings.GetInt("crosstalk-output-rate"), settings.GetInt("crosstalk-output-channels"))
	if err != nil {
		return worker.CrosstalkConfig{}, err
	}
	overflow, err := parseOverflow(settings.GetString("crosstalk-overflow"))
	if err != nil {
		return worker.CrosstalkConfig{}, err
	}
	return worker.CrosstalkConfig{
		Name:          name,
		ServerURL:     settings.GetString("crosstalk-url"),
		Token:         token,
		ClientName:    settings.GetString("crosstalk-client-name"),
		InputChannel:  settings.GetString("crosstalk-sink-channel"),
		OutputChannel: settings.GetString("crosstalk-source-channel"),
		OutputProfile: profile,
		Group:         settings.GetString("crosstalk-group"),
		Competing:     settings.GetBool("crosstalk-competing"),
		Queue:         settings.GetInt("crosstalk-queue"),
		Overflow:      overflow,
		FFmpeg:        ffmpeg,
		Reconnect:     settings.GetBool("crosstalk-reconnect"),
		MinBackoff:    settings.GetDuration("crosstalk-min-backoff"),
		MaxBackoff:    settings.GetDuration("crosstalk-max-backoff"),
		DisableMDNS:   settings.GetBool("crosstalk-disable-mdns"),
		DisableSTUN:   settings.GetBool("crosstalk-disable-stun"),
	}, nil
}

func parseOverflow(value string) (media.OverflowPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "drop", "drop-oldest":
		return media.OverflowDropOldest, nil
	case "fail":
		return media.OverflowFail, nil
	default:
		return 0, fmt.Errorf("unknown overflow policy %q", value)
	}
}

type healthPayload struct {
	Ready     bool   `json:"ready"`
	Reason    string `json:"reason,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Assigned  string `json:"assigned,omitempty"`
	Peer      string `json:"peer,omitempty"`
	Epoch     uint64 `json:"epoch,omitempty"`
}

func healthFromStage(health worker.CrosstalkHealth) healthPayload {
	return healthPayload{
		Ready:     health.Ready,
		Reason:    health.Reason,
		SessionID: health.SessionID,
		Assigned:  health.Assigned,
		Peer:      health.Peer,
		Epoch:     health.Epoch,
	}
}

func writeHealthFile(path string, health worker.CrosstalkHealth) error {
	if path == "" {
		return errors.New("health file path is required")
	}
	payload, err := json.Marshal(healthFromStage(health))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "qol-health-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func readHealthFile(path string) (healthPayload, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return healthPayload{}, err
	}
	var payload healthPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return healthPayload{}, err
	}
	return payload, nil
}

func watchHealth(ctx context.Context, path string, stage *worker.Crosstalk) {
	if path == "" || stage == nil {
		return
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	write := func() { _ = writeHealthFile(path, stage.Health()) }
	write()
	for {
		select {
		case <-ctx.Done():
			write()
			return
		case <-ticker.C:
			write()
		}
	}
}
