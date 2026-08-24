package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	qol "github.com/aleksclark/qol"
)

type CommandProcessor struct {
	command string
	args    []string
}

func NewCommandProcessor(command string, args ...string) *CommandProcessor {
	return &CommandProcessor{command: command, args: append([]string(nil), args...)}
}

func (p *CommandProcessor) Process(ctx context.Context, input io.Reader, output io.Writer) error {
	if p.command == "" {
		return errors.New("processor command is required")
	}
	command := exec.CommandContext(ctx, p.command, p.args...)
	command.Stdin = input
	command.Stdout = output
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("run %s: %w", p.command, err)
	}
	return nil
}

func (p *CommandProcessor) Start(ctx context.Context) (io.WriteCloser, io.ReadCloser, func() error, error) {
	if p.command == "" {
		return nil, nil, nil, errors.New("processor command is required")
	}
	command := exec.CommandContext(ctx, p.command, p.args...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, nil, err
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, nil, nil, fmt.Errorf("start %s: %w", p.command, err)
	}
	return stdin, stdout, command.Wait, nil
}

var _ liveProcessor = (*CommandProcessor)(nil)

type FFmpegOggOpusStore struct {
	ffmpeg    string
	directory string
	probe     []string
}

func NewFFmpegOggOpusStore(ffmpeg, directory string) *FFmpegOggOpusStore {
	return &FFmpegOggOpusStore{ffmpeg: ffmpeg, directory: directory}
}

func NewFFmpegS16LEOggOpusStore(ffmpeg, directory string, sampleRate, channels int) *FFmpegOggOpusStore {
	return &FFmpegOggOpusStore{
		ffmpeg:    ffmpeg,
		directory: directory,
		probe:     []string{"-f", "s16le", "-ar", strconv.Itoa(sampleRate), "-ac", strconv.Itoa(channels)},
	}
}

type ffmpegOggOpusWriter struct {
	command       *exec.Cmd
	stdin         io.WriteCloser
	temporaryPath string
	finalPath     string
	closed        bool
}

type existingAudioWriter struct {
	stored qol.StoredAudio
}

func (w *ffmpegOggOpusWriter) Write(data []byte) error {
	if w.closed {
		return errors.New("Ogg/Opus stream is closed")
	}
	_, err := w.stdin.Write(data)
	return err
}

func (w *ffmpegOggOpusWriter) Close() (qol.StoredAudio, error) {
	if w.closed {
		return qol.StoredAudio{}, errors.New("Ogg/Opus stream is closed")
	}
	w.closed = true
	if err := w.stdin.Close(); err != nil {
		return qol.StoredAudio{}, err
	}
	if err := w.command.Wait(); err != nil {
		return qol.StoredAudio{}, fmt.Errorf("encode Ogg/Opus: %w", err)
	}
	if err := os.Rename(w.temporaryPath, w.finalPath); err != nil {
		return qol.StoredAudio{}, err
	}
	info, err := os.Stat(w.finalPath)
	if err != nil {
		return qol.StoredAudio{}, err
	}
	return qol.StoredAudio{MediaType: "audio/ogg; codecs=opus", Path: w.finalPath, SizeBytes: uint64(info.Size())}, nil
}

func (w *ffmpegOggOpusWriter) Abort() error {
	if w.closed {
		return nil
	}
	w.closed = true
	_ = w.stdin.Close()
	if w.command.Process != nil {
		_ = w.command.Process.Kill()
	}
	_ = w.command.Wait()
	return os.Remove(w.temporaryPath)
}

func (w *existingAudioWriter) Write([]byte) error              { return nil }
func (w *existingAudioWriter) Close() (qol.StoredAudio, error) { return w.stored, nil }
func (w *existingAudioWriter) Abort() error                    { return nil }

func (s *FFmpegOggOpusStore) Open(ctx context.Context, sessionID string) (StreamAudioWriter, error) {
	if err := os.MkdirAll(s.directory, 0o750); err != nil {
		return nil, err
	}
	finalPath := filepath.Join(s.directory, SafeSessionName(sessionID)+".ogg")
	if info, err := os.Stat(finalPath); err == nil {
		return &existingAudioWriter{stored: qol.StoredAudio{MediaType: "audio/ogg; codecs=opus", Path: finalPath, SizeBytes: uint64(info.Size())}}, nil
	}
	temporary, err := os.CreateTemp(s.directory, SafeSessionName(sessionID)+"-*.ogg")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	args := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, s.probe...)
	args = append(args, "-i", "pipe:0", "-codec:a", "libopus", "-f", "ogg", "-flush_packets", "1", temporaryPath)
	command := exec.CommandContext(ctx, s.ffmpeg, args...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start Ogg/Opus encoder: %w", err)
	}
	return &ffmpegOggOpusWriter{command: command, stdin: stdin, temporaryPath: temporaryPath, finalPath: finalPath}, nil
}
