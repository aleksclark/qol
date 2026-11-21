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
	command.PersistentFlags().String("nats-url", nats.DefaultURL, "Comma-separated NATS server URLs")
	command.PersistentFlags().String("nats-user", "", "NATS user")
	command.PersistentFlags().String("nats-password", "", "NATS password; prefer QOL_NATS_PASSWORD")
	runCommand := &cobra.Command{Use: "run", Short: "Run one streaming graph stage", RunE: run}
	runCommand.Flags().String("type", "", "Stage type: capture, stt-whisper, prosody, translate, tts, conductor, or record")
	runCommand.Flags().String("stage", "", "Deprecated alias for --type")
	runCommand.Flags().String("name", "", "Stage instance name; defaults to the type")
	runCommand.Flags().String("input", "", "Input channel override")
	runCommand.Flags().String("output", "", "Output channel override")
	runCommand.Flags().String("processor", "", "Streaming processor executable")
	runCommand.Flags().StringSlice("processor-arg", nil, "Streaming processor argument")
	runCommand.Flags().Float64("reference-rate", prosody.DefaultReferenceRate, "Source-language reference speaking rate in syllables per second")
	runCommand.Flags().Duration("prosody-timeout", worker.DefaultProsodyTimeout, "Maximum wait for source audio covering an utterance")
	runCommand.Flags().Bool("attach-reference-audio", false, "Prosody: attach each utterance's source audio for synthesizers that condition on a reference")
	runCommand.Flags().Duration("playout-delay", worker.DefaultPlayoutDelay, "TTS output starts this long after the source utterance ends")
	runCommand.Flags().Float64("max-pace", worker.DefaultMaxPace, "Largest TTS speed-up used to catch up when output falls behind")
	runCommand.Flags().Duration("catch-up-window", worker.DefaultCatchUpWindow, "Backlog that triggers the largest TTS speed-up")
	runCommand.Flags().Duration("min-gap", worker.DefaultMinGap, "Shortest pause TTS leaves between phrases")
	runCommand.Flags().Bool("just-in-time", true, "TTS: hold translated text until shortly before its slot so it can still be condensed")
	runCommand.Flags().Duration("synthesis-lookahead", worker.DefaultSynthesisLookahead, "TTS: synthesize a phrase once its slot is this close to the source clock")
	runCommand.Flags().Duration("fade", worker.DefaultFade, "TTS: fade in and out at phrase edges")
	runCommand.Flags().Float64("prosody-smoothing", worker.DefaultProsodySmoothing, "TTS: weight of the newest delivery measurement (1 disables smoothing)")
	runCommand.Flags().Bool("coalesce", true, "TTS: speak consecutive fragments of one clause as a single phrase")
	runCommand.Flags().Duration("clause-hold", worker.DefaultClauseHold, "TTS: longest wait, on the source clock, for the rest of an unfinished clause")
	runCommand.Flags().Duration("sentence-gap", worker.DefaultSentenceGap, "TTS: pause after a sentence")
	runCommand.Flags().Duration("clause-gap", worker.DefaultClauseGap, "TTS: pause after a comma, colon, or semicolon")
	runCommand.Flags().Duration("continuation-gap", worker.DefaultContinuationGap, "TTS: pause between pieces of an unfinished clause")
	runCommand.Flags().Bool("trim-silence", true, "TTS: trim the synthesizer's own leading and trailing silence")
	runCommand.Flags().String("translator", "line", "Translate: line (one sentence per line) or llm (streaming context-aware translator)")
	runCommand.Flags().Duration("condense-lag", worker.DefaultCondenseLag, "Conductor: projected lag that triggers condensing the unspoken backlog")
	runCommand.Flags().Float64("condense-ratio", worker.DefaultCondenseRatio, "Conductor: target length of condensed speech relative to a full rendering")
	runCommand.Flags().Uint32("summarize-turns", worker.DefaultSummarizeTurns, "Conductor: translator context turns that trigger summarizing")
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
	connection, err := eventbus.Connect(eventbus.ConnectOptions{URLs: settings.GetString("nats-url"), User: settings.GetString("nats-user"), Password: settings.GetString("nats-password"), Name: "qol-worker-" + settings.GetString("type")})
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
		attachAudio:    settings.GetBool("attach-reference-audio"),
		speech: worker.SpeechOptions{
			Timeline: worker.Timeline{
				Delay:         settings.GetDuration("playout-delay"),
				MaxPace:       settings.GetFloat64("max-pace"),
				CatchUpWindow: settings.GetDuration("catch-up-window"),
				MinGap:        settings.GetDuration("min-gap"),
			},
			JustInTime:  settings.GetBool("just-in-time"),
			ClockStage:  "prosody",
			Lookahead:   settings.GetDuration("synthesis-lookahead"),
			Fade:        settings.GetDuration("fade"),
			Smoothing:   settings.GetFloat64("prosody-smoothing"),
			Coalesce:    settings.GetBool("coalesce"),
			ClauseHold:  settings.GetDuration("clause-hold"),
			TrimSilence: settings.GetBool("trim-silence"),
			Gaps: worker.PhraseGaps{
				Sentence:     settings.GetDuration("sentence-gap"),
				Clause:       settings.GetDuration("clause-gap"),
				Continuation: settings.GetDuration("continuation-gap"),
			},
		},
		translator: settings.GetString("translator"),
		conductor: worker.ConductorOptions{
			CondenseLag:    settings.GetDuration("condense-lag"),
			CondenseRatio:  float32(settings.GetFloat64("condense-ratio")),
			SummarizeTurns: settings.GetUint32("summarize-turns"),
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
	attachAudio    bool
	speech         worker.SpeechOptions
	translator     string
	conductor      worker.ConductorOptions
}

func buildStage(options stageOptions) (qol.Stage, error) {
	name := options.name
	if name == "" {
		name = options.stageType
	}
	switch options.stageType {
	case "capture":
		return worker.NewCapture(), nil
	case "conductor":
		return worker.NewConductor(options.conductor), nil
	case "prosody":
		var emotion worker.LiveProcessor
		if options.processor != "" {
			emotion = worker.NewCommandProcessor(options.processor, options.args...)
		}
		return worker.NewProsody(media.NewDecoder(options.ffmpeg), emotion, options.referenceRate, options.prosodyTimeout).AttachReferenceAudio(options.attachAudio), nil
	case "stt-whisper", "translate", "tts":
		if options.processor == "" {
			return nil, fmt.Errorf("%s requires --processor or QOL_PROCESSOR", options.stageType)
		}
		processor := worker.NewCommandProcessor(options.processor, options.args...)
		switch options.stageType {
		case "stt-whisper":
			return worker.NewSTT(processor), nil
		case "translate":
			switch options.translator {
			case "", "line":
				return worker.NewTranslate(processor), nil
			case "llm":
				return worker.NewStreamingTranslate(processor), nil
			default:
				return nil, fmt.Errorf("translator must be line or llm, got %q", options.translator)
			}
		default:
			return worker.NewTTS(processor, options.speech), nil
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
		return nil, errors.New("stage type must be capture, stt-whisper, prosody, translate, tts, conductor, or record")
	}
}
