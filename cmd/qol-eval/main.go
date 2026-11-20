package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/config"
	"github.com/aleksclark/qol/internal/evaluation"
	"github.com/aleksclark/qol/internal/wire"
	"github.com/aleksclark/qol/internal/worker"
	"github.com/spf13/cobra"
)

func main() {
	if err := newCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	command := &cobra.Command{Use: "qol-eval", Short: "Evaluate how well synthesized speech carries source delivery"}
	carry := &cobra.Command{
		Use:   "carry-through",
		Short: "Score synthesized audio against the source prosody in a synthesis fixture",
		RunE:  runCarryThrough,
	}
	carry.Flags().String("requests", "", "Varint-delimited SynthesisRequest frames (QOL_E2E_DUMP_SYNTHESIS output)")
	carry.Flags().String("audio", "", "Varint-delimited SynthesisChunk frames produced for those requests")
	carry.Flags().String("emotion-processor", "", "Optional emotion classifier executable for output emotion agreement")
	carry.Flags().StringSlice("emotion-processor-arg", nil, "Emotion classifier argument")
	carry.Flags().String("ffmpeg", "ffmpeg", "ffmpeg executable used to resample audio for the classifier")
	carry.Flags().Float64("min-rate-correlation", -1, "Fail below this speaking-rate correlation; -1 disables")
	carry.Flags().Float64("min-pitch-correlation", -1, "Fail below this pitch correlation; -1 disables")
	carry.Flags().Bool("verbose", false, "Print one line per utterance")
	command.AddCommand(carry)
	return command
}

func runCarryThrough(command *cobra.Command, _ []string) error {
	settings, err := config.Bind(command)
	if err != nil {
		return err
	}
	requests, err := os.Open(settings.GetString("requests"))
	if err != nil {
		return err
	}
	defer requests.Close()
	audio, err := os.Open(settings.GetString("audio"))
	if err != nil {
		return err
	}
	defer audio.Close()
	utterances, err := evaluation.FromSynthesis(requests, audio)
	if err != nil {
		return err
	}
	var classify evaluation.Classifier
	if processor := settings.GetString("emotion-processor"); processor != "" {
		var stop func()
		classify, stop, err = startClassifier(command.Context(), processor, settings.GetStringSlice("emotion-processor-arg"), settings.GetString("ffmpeg"))
		if err != nil {
			return err
		}
		defer stop()
	}
	report, err := evaluation.CarryThrough(utterances, classify)
	if err != nil {
		return err
	}
	output := command.OutOrStdout()
	if settings.GetBool("verbose") {
		for _, row := range report.Rows {
			fmt.Fprintln(output, row)
		}
	}
	fmt.Fprintln(output, report.Summary())
	if minimum := settings.GetFloat64("min-rate-correlation"); minimum > -1 && !(report.Rate >= minimum) {
		return fmt.Errorf("speaking-rate correlation %.2f is below %.2f", report.Rate, minimum)
	}
	if minimum := settings.GetFloat64("min-pitch-correlation"); minimum > -1 && !(report.Pitch >= minimum) {
		return fmt.Errorf("pitch correlation %.2f is below %.2f", report.Pitch, minimum)
	}
	return nil
}

func startClassifier(ctx context.Context, processor string, args []string, ffmpeg string) (evaluation.Classifier, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	input, output, wait, err := worker.NewCommandProcessor(processor, args...).Start(ctx)
	if err != nil {
		return nil, nil, err
	}
	reader := bufio.NewReader(output)
	stop := func() {
		_ = input.Close()
		_ = wait()
	}
	classify := func(samples []int16, sampleRate int) (qol.EmotionResult, error) {
		resampled, err := resample(ffmpeg, samples, sampleRate)
		if err != nil {
			return qol.EmotionResult{}, err
		}
		if err := wire.WriteEmotionRequest(input, qol.Audio{Format: qol.PCMFormat{SampleRate: worker.ProsodySampleRate, Channels: 1, Format: qol.SampleS16LE}, Data: resampled}); err != nil {
			return qol.EmotionResult{}, err
		}
		return wire.ReadEmotionResult(reader)
	}
	return classify, stop, nil
}

func resample(ffmpeg string, samples []int16, sampleRate int) ([]byte, error) {
	raw := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(raw[index*2:], uint16(sample))
	}
	if sampleRate == worker.ProsodySampleRate {
		return raw, nil
	}
	command := exec.Command(ffmpeg, "-v", "error", "-f", "s16le", "-ar", strconv.Itoa(sampleRate), "-ac", "1", "-i", "pipe:0", "-f", "s16le", "-ar", strconv.Itoa(worker.ProsodySampleRate), "-ac", "1", "pipe:1")
	command.Stdin = bytes.NewReader(raw)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	resampled, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("resample: %w: %s", err, stderr.String())
	}
	return resampled, nil
}
