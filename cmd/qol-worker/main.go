package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	qol "github.com/aleksclark/qol"
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
	runCommand := &cobra.Command{Use: "run", Short: "Run one streaming pipeline stage", RunE: run}
	runCommand.Flags().String("type", "", "Stage type: capture, stt-whisper, translate, tts, or record")
	runCommand.Flags().String("stage", "", "Deprecated alias for --type")
	runCommand.Flags().String("name", "", "Stage instance name; defaults to the type")
	runCommand.Flags().String("input", "", "Input channel override")
	runCommand.Flags().String("output", "", "Output channel override")
	runCommand.Flags().String("processor", "", "Streaming processor executable")
	runCommand.Flags().StringSlice("processor-arg", nil, "Streaming processor argument")
	runCommand.Flags().String("spool-dir", "/tmp/qol", "Pipeline spool directory")
	runCommand.Flags().String("output-dir", "/output", "Ogg/Opus output directory")
	runCommand.Flags().String("ffmpeg", "ffmpeg", "ffmpeg executable")
	command.AddCommand(runCommand)
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
	stage, err := buildStage(stageType, settings.GetString("name"), settings.GetString("input"), settings.GetString("output"), settings.GetString("processor"), settings.GetStringSlice("processor-arg"), settings.GetString("spool-dir"), settings.GetString("output-dir"), settings.GetString("ffmpeg"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.Info("Qol worker running", "type", stageType, "name", stage.Spec().Name)
	return stage.Run(ctx, bus)
}

func buildStage(stageType, name, input, output, processor string, args []string, spoolDir, outputDir, ffmpeg string) (qol.Stage, error) {
	if name == "" {
		name = stageType
	}
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
	default:
		return nil, errors.New("stage type must be capture, stt-whisper, translate, tts, or record")
	}
}
