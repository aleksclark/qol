package media

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	qol "github.com/aleksclark/qol"
)

type InboundConfig struct {
	FFmpeg   string
	Codec    Codec
	Profile  Profile
	Queue    int
	Overflow OverflowPolicy
}

type Inbound struct {
	cfg          InboundConfig
	metrics      *Metrics
	queue        *Queue[Frame]
	chunks       chan OutputChunk
	cancel       context.CancelFunc
	ready        chan struct{}
	done         chan struct{}
	err          error
	mu           sync.Mutex
	lastSeq      uint16
	lastTS       uint32
	haveSeq      bool
	closed       bool
	outputCursor atomic.Int64
}

func NewInbound(cfg InboundConfig) (*Inbound, error) {
	if err := ValidateCodec(cfg.Codec); err != nil {
		return nil, err
	}
	if err := ValidateProfile(cfg.Profile); err != nil {
		return nil, err
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	return &Inbound{
		cfg:     cfg,
		metrics: &Metrics{},
		queue:   NewQueue[Frame](cfg.Queue, cfg.Overflow),
		chunks:  make(chan OutputChunk, 16),
	}, nil
}

func (i *Inbound) Metrics() *Metrics          { return i.metrics }
func (i *Inbound) Chunks() <-chan OutputChunk { return i.chunks }
func (i *Inbound) Ready() <-chan struct{}     { return i.ready }
func (i *Inbound) Done() <-chan struct{}      { return i.done }

// Start waits until FFmpeg has started and bound its RTP input. A nil result
// means the converter is operational, not merely that its goroutine launched.
func (i *Inbound) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	i.cancel = cancel
	i.ready = make(chan struct{})
	i.done = make(chan struct{})
	i.outputCursor.Store(0)
	go func() {
		err := i.decode(runCtx)
		i.mu.Lock()
		i.err = err
		i.mu.Unlock()
		close(i.done)
	}()
	select {
	case <-i.ready:
		select {
		case <-i.done:
			return i.Err()
		default:
			return nil
		}
	case <-i.done:
		return i.Err()
	case <-ctx.Done():
		cancel()
		<-i.done
		return ctx.Err()
	}
}

func (i *Inbound) Err() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.err
}

func (i *Inbound) WriteFrame(frame Frame) error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return ErrClosed
	}
	if !frame.End && i.haveSeq {
		if frame.Sequence != i.lastSeq+1 && frame.Sequence != 0 {
			i.metrics.Discontinuities.Add(1)
			if int16(frame.Sequence-i.lastSeq) < 0 {
				i.metrics.LatePackets.Add(1)
			}
		}
		if frame.Timestamp < i.lastTS {
			i.metrics.LatePackets.Add(1)
		}
	}
	if !frame.End {
		i.lastSeq = frame.Sequence
		i.lastTS = frame.Timestamp
		i.haveSeq = true
	}
	i.mu.Unlock()
	i.metrics.FramesIn.Add(1)
	if err := i.queue.Push(frame); err != nil {
		if err == ErrOverflow {
			i.metrics.Dropped.Add(1)
		}
		return err
	}
	i.metrics.Dropped.Store(i.queue.Dropped())
	return nil
}

func (i *Inbound) Close() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	i.mu.Unlock()
	i.queue.Close()
	if i.cancel != nil {
		i.cancel()
	}
	if i.done == nil {
		return nil
	}
	<-i.done
	return i.Err()
}

func (i *Inbound) decode(ctx context.Context) error {
	defer close(i.chunks)
	port, err := freeUDPPort()
	if err != nil {
		return err
	}
	proc, err := startProcess(ctx, i.cfg.FFmpeg, inboundArgs(i.cfg.Profile)...)
	if err != nil {
		return err
	}
	defer proc.Kill()
	if _, err := io.WriteString(proc.stdin, opusSDP(port, i.cfg.Codec)); err != nil {
		return err
	}
	if err := proc.stdin.Close(); err != nil {
		return err
	}
	if err := waitUDPBound(ctx, port); err != nil {
		return wrapProcess(err, proc)
	}
	close(i.ready)
	conn, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	defer conn.Close()

	readDone := make(chan emitResult, 1)
	go func() {
		readDone <- i.emitOutput(ctx, proc.stdout)
	}()
	if err := i.forwardRTP(conn); err != nil && ctx.Err() == nil {
		i.metrics.ProcessExits.Add(1)
		return wrapProcess(err, proc)
	}
	_ = conn.Close()
	select {
	case last, ok := <-readDone:
		if !ok {
			break
		}
		if last.err != nil {
			i.metrics.ProcessExits.Add(1)
			return wrapProcess(last.err, proc)
		}
		select {
		case i.chunks <- OutputChunk{End: true, Span: qol.Span{Start: last.cursor, End: last.cursor}}:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}
	select {
	case i.chunks <- OutputChunk{End: true, Span: i.currentOutputSpan()}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (i *Inbound) forwardRTP(conn net.Conn) error {
	seq := uint16(0)
	ts := uint32(0)
	for {
		frame, ok, err := i.queue.Pop()
		if err != nil {
			return err
		}
		if !ok || frame.End {
			return nil
		}
		if frame.Sequence == 0 && frame.Timestamp == 0 {
			frame.Sequence = seq
			frame.Timestamp = ts
			seq++
			ts += OpusFrameSamples
		}
		if len(frame.Payload) == 0 {
			continue
		}
		data, err := PackRTP(frame).Marshal()
		if err != nil {
			return err
		}
		if _, err := conn.Write(data); err != nil {
			return err
		}
	}
}

type emitResult struct {
	cursor qol.MediaTime
	err    error
}

func (i *Inbound) emitOutput(ctx context.Context, stdout io.Reader) emitResult {
	if i.cfg.Profile.Kind == ProfileOggOpus {
		return i.emitOggOutput(ctx, stdout)
	}

	rate := i.cfg.Profile.SampleRate
	if rate == 0 {
		rate = OpusClockRate
	}
	buf := make([]byte, 4096)
	var cursor qol.MediaTime
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			samples := n / max(1, i.cfg.Profile.Channels*BytesPerSample(i.cfg.Profile.SampleFormat()))
			span := SpanFromSamples(cursor, samples, rate)
			cursor = span.End
			i.outputCursor.Store(int64(cursor))
			i.metrics.FramesOut.Add(1)
			select {
			case i.chunks <- OutputChunk{Data: data, Span: span}:
			case <-ctx.Done():
				return emitResult{cursor: cursor, err: ctx.Err()}
			}
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				return emitResult{cursor: cursor, err: err}
			}
			return emitResult{cursor: cursor}
		}
	}
}

// emitOggOutput emits whole pages so each payload is independently framed at a
// container boundary. Its spans use the Opus granule clock; headers and pages
// without completed packets have zero duration.
func (i *Inbound) emitOggOutput(ctx context.Context, stdout io.Reader) emitResult {
	pages := oggPageReader{}
	for {
		page, granule, err := pages.readPage(stdout)
		if len(page) > 0 {
			span := pages.span(page, granule)
			i.outputCursor.Store(int64(pages.cursor))
			i.metrics.FramesOut.Add(1)
			select {
			case i.chunks <- OutputChunk{Data: page, Span: span}:
			case <-ctx.Done():
				return emitResult{cursor: pages.cursor, err: ctx.Err()}
			}
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				return emitResult{cursor: pages.cursor, err: err}
			}
			return emitResult{cursor: pages.cursor}
		}
	}
}

func (i *Inbound) currentOutputSpan() qol.Span {
	cursor := qol.MediaTime(i.outputCursor.Load())
	return qol.Span{Start: cursor, End: cursor}
}

func inboundArgs(profile Profile) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-protocol_whitelist", "pipe,file,udp,rtp", "-probesize", "32", "-analyzeduration", "0", "-fflags", "+nobuffer", "-f", "sdp", "-i", "pipe:0"}
	switch profile.Kind {
	case ProfileOggOpus:
		args = append(args, "-c:a", "copy", "-f", "ogg", "-flush_packets", "1", "-page_duration", "20000", "pipe:1")
	case ProfilePCMF32LE:
		args = append(args, "-f", "f32le", "-ar", strconv.Itoa(profile.SampleRate), "-ac", strconv.Itoa(profile.Channels), "pipe:1")
	default:
		args = append(args, "-f", "s16le", "-ar", strconv.Itoa(profile.SampleRate), "-ac", strconv.Itoa(profile.Channels), "pipe:1")
	}
	return args
}

func opusSDP(port int, codec Codec) string {
	channels := int(codec.Channels)
	if channels == 0 {
		channels = 2
	}
	return fmt.Sprintf("v=0\no=- 0 0 IN IP4 127.0.0.1\ns=Qol\nc=IN IP4 127.0.0.1\nt=0 0\nm=audio %d RTP/AVP %d\na=rtpmap:%d opus/%d/%d\n", port, DefaultPayloadType, DefaultPayloadType, OpusClockRate, channels)
}

func freeUDPPort() (int, error) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	if err := probe.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func waitUDPBound(ctx context.Context, port int) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		probe, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return nil
		}
		_ = probe.Close()
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("ffmpeg did not bind udp port %d", port)
}

func (p Profile) String() string {
	if p.Kind == ProfileOggOpus {
		return string(p.Kind)
	}
	return fmt.Sprintf("%s/%d/%d", p.Kind, p.SampleRate, p.Channels)
}
