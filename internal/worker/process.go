package worker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"
)

type persistentProcess struct {
	starter LiveProcessor
	mu      sync.Mutex
	input   io.WriteCloser
	output  io.ReadCloser
	reader  *bufio.Reader
	wait    func() error
	cancel  context.CancelFunc
}

func newPersistentProcess(starter LiveProcessor) *persistentProcess {
	return &persistentProcess{starter: starter}
}

func (p *persistentProcess) exchange(ctx context.Context, request func(io.Writer) error, response func(*bufio.Reader) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.input == nil {
		if err := p.startLocked(ctx); err != nil {
			return err
		}
	}
	if err := request(p.input); err != nil {
		p.stopLocked()
		return err
	}
	if err := response(p.reader); err != nil {
		p.stopLocked()
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

func (p *persistentProcess) startLocked(ctx context.Context) error {
	processContext, cancel := context.WithCancel(ctx)
	input, output, wait, err := p.starter.Start(processContext)
	if err != nil {
		cancel()
		return err
	}
	p.input, p.output, p.reader, p.wait, p.cancel = input, output, bufio.NewReader(output), wait, cancel
	return nil
}

func (p *persistentProcess) stopLocked() {
	if p.input == nil {
		return
	}
	_ = p.input.Close()
	p.cancel()
	_ = p.output.Close()
	_ = p.wait()
	p.input, p.output, p.reader, p.wait, p.cancel = nil, nil, nil, nil, nil
}

func (p *persistentProcess) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}
