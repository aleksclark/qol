package worker_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleksclark/qol/internal/worker"
)

func TestFFmpegOggOpusStoreStreamsOpusAndRetries(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	directory := t.TempDir()
	store := worker.NewFFmpegOggOpusStore("ffmpeg", directory)
	writer, err := store.Open(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	wav := generatedWAV(t)
	middle := len(wav) / 2
	if err := writer.Write(wav[:middle]); err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(wav[middle:]); err != nil {
		t.Fatal(err)
	}
	stored, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stored.MediaType != "audio/ogg; codecs=opus" || stored.Path != filepath.Join(directory, "session.ogg") || stored.SizeBytes == 0 {
		t.Fatalf("unexpected stored audio: %#v", stored)
	}
	probe := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "default=noprint_wrappers=1:nokey=1", stored.Path)
	codec, err := probe.Output()
	if err != nil || strings.TrimSpace(string(codec)) != "opus" {
		t.Fatalf("invalid Ogg/Opus: %q, %v", codec, err)
	}
	retry, err := store.Open(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := retry.Write(nil); err != nil {
		t.Fatal(err)
	}
	retried, err := retry.Close()
	if err != nil || retried != stored {
		t.Fatalf("retry changed output: %#v, %v", retried, err)
	}
}

func TestFFmpegS16LEOggOpusStoreConcatenatesPCM(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	directory := t.TempDir()
	store := worker.NewFFmpegS16LEOggOpusStore("ffmpeg", directory, 22050, 1)
	writer, err := store.Open(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	pcm := generatedPCM(t)
	middle := len(pcm) / 2
	if err := writer.Write(pcm[:middle]); err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(pcm[middle:]); err != nil {
		t.Fatal(err)
	}
	stored, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stored.MediaType != "audio/ogg; codecs=opus" || stored.Path != filepath.Join(directory, "session.ogg") || stored.SizeBytes == 0 {
		t.Fatalf("unexpected stored audio: %#v", stored)
	}
	probe := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "default=noprint_wrappers=1:nokey=1", stored.Path)
	codec, err := probe.Output()
	if err != nil || strings.TrimSpace(string(codec)) != "opus" {
		t.Fatalf("invalid Ogg/Opus: %q, %v", codec, err)
	}
}

func generatedWAV(t *testing.T) []byte {
	t.Helper()
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.1", "-ar", "22050", "-ac", "1", "-f", "wav", "pipe:1")
	wav, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return wav
}

func generatedPCM(t *testing.T) []byte {
	t.Helper()
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.1", "-ar", "22050", "-ac", "1", "-f", "s16le", "pipe:1")
	pcm, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return pcm
}
