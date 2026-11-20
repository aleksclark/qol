package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/config"
	"github.com/aleksclark/qol/internal/eventbus"
	"github.com/aleksclark/qol/internal/media"
	"github.com/aleksclark/qol/internal/prosody"
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
	runCommand := &cobra.Command{Use: "run", Short: "Run one streaming graph stage", RunE: run}
	runCommand.Flags().String("type", "", "Stage type: capture, stt-whisper, prosody, translate, tts, or record")
	runCommand.Flags().String("stage", "", "Deprecated alias for --type")
	runCommand.Flags().String("name", "", "Stage instance name; defaults to the type")
	runCommand.Flags().String("input", "", "Input channel override")
	runCommand.Flags().String("output", "", "Output channel override")
	runCommand.Flags().String("processor", "", "Streaming processor executable")
	runCommand.Flags().StringSlice("processor-arg", nil, "Streaming processor argument")
	runCommand.Flags().Float64("reference-rate", prosody.DefaultReferenceRate, "Source-language reference speaking rate in syllables per second")
	runCommand.Flags().Duration("prosody-timeout", worker.DefaultProsodyTimeout, "Maximum wait for source audio covering an utterance")
	runCommand.Flags().Duration("playout-delay", worker.DefaultPlayoutDelay, "TTS output starts this long after the source utterance ends")
	runCommand.Flags().Float64("max-pace", worker.DefaultMaxPace, "Largest TTS speed-up used to catch up when output falls behind")
	runCommand.Flags().Duration("catch-up-window", worker.DefaultCatchUpWindow, "Backlog that triggers the largest TTS speed-up")
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
	stage, err := buildStage(stageOptions{
		stageType:      stageType,
		name:           settings.GetString("name"),
		input:          settings.GetString("input"),
		output:         settings.GetString("output"),
		processor:      settings.GetString("processor"),
		args:           settings.GetStringSlice("processor-arg"),
		outputDir:      settings.GetString("output-dir"),
		ffmpeg:         settings.GetString("ffmpeg"),
		referenceRate:  settings.GetFloat64("reference-rate"),
		prosodyTimeout: settings.GetDuration("prosody-timeout"),
		timeline: worker.Timeline{
			Delay:         settings.GetDuration("playout-delay"),
			MaxPace:       settings.GetFloat64("max-pace"),
			CatchUpWindow: settings.GetDuration("catch-up-window"),
		},
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.Info("Qol worker running", "type", stageType, "name", stage.Spec().Name)
	return stage.Run(ctx, bus)
}

type stageOptions struct {
	stageType      string
	name           string
	input          string
	output         string
	processor      string
	args           []string
	outputDir      string
	ffmpeg         string
	referenceRate  float64
	prosodyTimeout time.Duration
	timeline       worker.Timeline
}

func buildStage(options stageOptions) (qol.Stage, error) {
	name := options.name
	if name == "" {
		name = options.stageType
	}
	switch options.stageType {
	case "capture":
		return worker.NewCapture(), nil
	case "prosody":
		var emotion worker.LiveProcessor
		if options.processor != "" {
			emotion = worker.NewCommandProcessor(options.processor, options.args...)
		}
		return worker.NewProsody(media.NewDecoder(options.ffmpeg), emotion, options.referenceRate, options.prosodyTimeout), nil
	case "stt-whisper", "translate", "tts":
		if options.processor == "" {
			return nil, fmt.Errorf("%s requires --processor or QOL_PROCESSOR", options.stageType)
		}
		processor := worker.NewCommandProcessor(options.processor, options.args...)
		switch options.stageType {
		case "stt-whisper":
			return worker.NewSTT(processor), nil
		case "translate":
			return worker.NewTranslate(processor), nil
		default:
			return worker.NewTTS(processor, options.timeline), nil
		}
	case "record":
		if options.input == "" || options.output == "" {
			return nil, fmt.Errorf("record instance %s requires --input and --output", name)
		}
		store := worker.NewFFmpegOggOpusStore(options.ffmpeg, options.outputDir)
		if options.input == qol.ChannelTTSAudio {
			store = worker.NewFFmpegS16LEOggOpusStore(options.ffmpeg, options.outputDir, worker.SynthesisSampleRate, 1)
		}
		return worker.NewRecord(name, options.input, options.output, store), nil
	default:
		return nil, errors.New("stage type must be capture, stt-whisper, prosody, translate, tts, or record")
	}
}
