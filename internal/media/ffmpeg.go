package media

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

const (
	SampleRate  = 16000
	Channels    = 1
	ChunkFrames = 320
)

type Decoder struct {
	binary string
}

func NewDecoder(binary string) *Decoder {
	return &Decoder{binary: binary}
}

func (d *Decoder) Start(ctx context.Context) (io.WriteCloser, io.ReadCloser, func() error, error) {
	command := exec.CommandContext(ctx, d.binary, "-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-f", "s16le", "-ar", "16000", "-ac", "1", "pipe:1")
	input, err := command.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := command.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	return input, output, command.Wait, nil
}
