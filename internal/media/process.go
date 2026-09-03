package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
)

type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr bytes.Buffer
	wait   sync.Once
	err    error
}

func startProcess(ctx context.Context, binary string, args ...string) (*process, error) {
	if binary == "" {
		binary = "ffmpeg"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	proc := &process{cmd: cmd, stdin: stdin, stdout: stdout}
	cmd.Stderr = &proc.stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}
	return proc, nil
}

func (p *process) Close() error {
	if p == nil {
		return nil
	}
	_ = p.stdin.Close()
	return p.Wait()
}

func (p *process) Kill() {
	if p == nil || p.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	_ = p.Wait()
}

func (p *process) Wait() error {
	p.wait.Do(func() {
		p.err = p.cmd.Wait()
	})
	return p.err
}

func (p *process) Error() error {
	if p == nil {
		return nil
	}
	err := p.Wait()
	if err == nil {
		return nil
	}
	detail := bytes.TrimSpace(p.stderr.Bytes())
	if len(detail) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

// probeProcess proves that the configured executable can start without
// exposing its command line or stderr to health callers.
func probeProcess(ctx context.Context, binary string) error {
	proc, err := startProcess(ctx, binary, "-version")
	if err != nil {
		return err
	}
	// Process creation is the readiness contract. Its terminal state belongs to
	// the actual conversion and is exposed through Done and Err.
	proc.Kill()
	return nil
}
