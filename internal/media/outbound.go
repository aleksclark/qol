package media

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pion/rtp"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/wire"
)

type OutboundConfig struct {
	FFmpeg   string
	Codec    Codec
	Queue    int
	Overflow OverflowPolicy
}

type Outbound struct {
	cfg     OutboundConfig
	metrics *Metrics
	queue   *Queue[outboundItem]
	frames  chan Frame
	cancel  context.CancelFunc
	ready   chan struct{}
	done    chan struct{}
	err     error
	mu      sync.Mutex
	format  qol.PCMFormat
	hasPCM  bool
	encoded EncodedInput
	closed  bool
}

type outboundItem struct {
	data    []byte
	format  qol.PCMFormat
	kind    ProfileKind
	encoded EncodedInput
	span    qol.Span
	end     bool
}

func NewOutbound(cfg OutboundConfig) (*Outbound, error) {
	if err := ValidateCodec(cfg.Codec); err != nil {
		return nil, err
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	return &Outbound{
		cfg:     cfg,
		metrics: &Metrics{},
		queue:   NewQueue[outboundItem](cfg.Queue, cfg.Overflow),
		frames:  make(chan Frame, 16),
	}, nil
}

func (o *Outbound) Metrics() *Metrics      { return o.metrics }
func (o *Outbound) Ready() <-chan struct{} { return o.ready }
func (o *Outbound) Done() <-chan struct{}  { return o.done }

// Start verifies that FFmpeg can actually execute before accepting media. The
// encoder itself starts with the first event because its input format is part
// of that event.
func (o *Outbound) Start(ctx context.Context) error {
	if err := probeProcess(ctx, o.cfg.FFmpeg); err != nil {
		o.mu.Lock()
		o.err = err
		o.mu.Unlock()
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	o.cancel = cancel
	o.ready = make(chan struct{})
	o.done = make(chan struct{})
	close(o.ready)
	go func() {
		err := o.encode(runCtx)
		o.mu.Lock()
		o.err = err
		o.mu.Unlock()
		close(o.done)
	}()
	return nil
}

func (o *Outbound) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *Outbound) WriteEvent(event qol.Event) error {
	item, err := decodeOutbound(event)
	if err != nil {
		return err
	}
	return o.enqueue(item)
}

func (o *Outbound) WritePCM(audio qol.Audio, span qol.Span, end bool) error {
	if audio.Format.SampleRate <= 0 || audio.Format.Channels <= 0 || (len(audio.Data) == 0 && !end) {
		return ErrMalformedMedia
	}
	return o.enqueue(outboundItem{data: append([]byte(nil), audio.Data...), format: audio.Format, kind: pcmKind(audio.Format.Format), span: span, end: end})
}

func (o *Outbound) WriteStream(mediaType string, data []byte, end bool) error {
	if len(data) == 0 && !end {
		return ErrMalformedMedia
	}
	encoded, err := ParseEncodedInputMediaType(mediaType)
	if err != nil {
		return err
	}
	return o.enqueue(outboundItem{data: append([]byte(nil), data...), encoded: encoded, end: end})
}

func (o *Outbound) Frames() <-chan Frame { return o.frames }

func (o *Outbound) Close() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	o.mu.Unlock()
	o.queue.Close()
	if o.cancel != nil {
		o.cancel()
	}
	return o.Wait()
}

// Wait waits for an EOS-terminated stream to finish producing RTP frames.
// Callers must not call Wait concurrently with Close.
func (o *Outbound) Wait() error {
	if o.done == nil {
		return o.Err()
	}
	<-o.done
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	return o.err
}

func (o *Outbound) enqueue(item outboundItem) error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return ErrClosed
	}
	if item.encoded.MediaType != "" {
		if o.hasPCM || (o.encoded.MediaType != "" && o.encoded.MediaType != item.encoded.MediaType) {
			o.mu.Unlock()
			return ErrFormatChanged
		}
		o.encoded = item.encoded
	} else if o.encoded.MediaType != "" || (o.hasPCM && !SamePCMFormat(o.format, item.format) && len(item.data) > 0) {
		o.mu.Unlock()
		return ErrFormatChanged
	} else if len(item.data) > 0 {
		o.format = item.format
		o.hasPCM = true
	}
	o.mu.Unlock()
	if err := o.queue.Push(item); err != nil {
		if err == ErrOverflow {
			o.metrics.Dropped.Add(1)
		}
		return err
	}
	o.metrics.Dropped.Store(o.queue.Dropped())
	return nil
}

func (o *Outbound) encode(ctx context.Context) error {
	defer close(o.frames)
	item, ok, err := o.queue.Pop()
	if err != nil {
		return err
	}
	if !ok {
		return ctx.Err()
	}
	if len(item.data) == 0 && item.end {
		select {
		case o.frames <- Frame{End: true}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return o.pipe(ctx, item)
}

func (o *Outbound) pipe(ctx context.Context, first outboundItem) error {
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	proc, err := startProcess(ctx, o.cfg.FFmpeg, outboundArgs(first, listener.LocalAddr().String())...)
	if err != nil {
		return err
	}
	defer proc.Kill()
	go io.Copy(io.Discard, proc.stdout)

	fed := make(chan error, 1)
	go func() {
		fed <- o.feed(proc, first)
	}()

	readDone := make(chan error, 1)
	go func() {
		readDone <- o.collectRTP(ctx, listener, fed)
	}()

	select {
	case err := <-readDone:
		if err != nil {
			o.metrics.ProcessExits.Add(1)
			return wrapProcess(err, proc)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	if waitErr := proc.Wait(); waitErr != nil {
		o.metrics.ProcessExits.Add(1)
		return wrapProcess(waitErr, proc)
	}
	select {
	case o.frames <- Frame{End: true}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (o *Outbound) feed(proc *process, first outboundItem) error {
	if len(first.data) > 0 {
		if _, err := proc.stdin.Write(first.data); err != nil {
			return err
		}
	}
	if first.end {
		return proc.stdin.Close()
	}
	for {
		item, ok, err := o.queue.Pop()
		if err != nil {
			return err
		}
		if !ok {
			return proc.stdin.Close()
		}
		if !sameOutboundFormat(first, item) {
			return ErrFormatChanged
		}
		if len(item.data) > 0 {
			if _, err := proc.stdin.Write(item.data); err != nil {
				return err
			}
		}
		if item.end {
			return proc.stdin.Close()
		}
	}
}

func (o *Outbound) collectRTP(ctx context.Context, listener net.PacketConn, fed <-chan error) error {
	buf := make([]byte, 2048)
	idle := 0
	inputClosed := false
	for {
		_ = listener.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _, err := listener.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case feedErr := <-fed:
				inputClosed = true
				if feedErr != nil {
					return feedErr
				}
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if inputClosed {
					idle++
					if idle > 5 {
						return nil
					}
				}
				continue
			}
			return err
		}
		idle = 0
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		o.metrics.FramesOut.Add(1)
		select {
		case o.frames <- Frame{
			Payload:     append([]byte(nil), pkt.Payload...),
			Sequence:    pkt.SequenceNumber,
			Timestamp:   pkt.Timestamp,
			Marker:      pkt.Marker,
			PayloadType: pkt.PayloadType,
			SSRC:        pkt.SSRC,
		}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func outboundArgs(item outboundItem, rtpURL string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-fflags", "+nobuffer"}
	if item.encoded.MediaType != "" {
		args = append(args, "-probesize", "2048", "-analyzeduration", "0", "-f", item.encoded.Demuxer)
	} else {
		format := "s16le"
		if item.format.Format == qol.SampleF32LE {
			format = "f32le"
		}
		args = append(args,
			"-f", format,
			"-ar", strconv.Itoa(item.format.SampleRate),
			"-ac", strconv.Itoa(item.format.Channels),
		)
	}
	args = append(args,
		"-i", "pipe:0",
		"-ac", "1",
		"-ar", strconv.Itoa(OpusClockRate),
		"-c:a", "libopus",
		"-application", "audio",
		"-frame_duration", strconv.Itoa(OpusFrameMs),
		"-vbr", "on",
		"-b:a", "64k",
		"-payload_type", strconv.Itoa(DefaultPayloadType),
		"-f", "rtp", "rtp://"+rtpURL,
	)
	return args
}

func decodeOutbound(event qol.Event) (outboundItem, error) {
	switch event.Type {
	case qol.TypeAudioPCM, qol.TypeAudioPCMFinal:
		audio, err := wire.UnmarshalAudio(event.Payload)
		if err != nil {
			return outboundItem{}, err
		}
		return outboundItem{data: audio.Data, format: audio.Format, kind: pcmKind(audio.Format.Format), span: event.Span, end: event.Type == qol.TypeAudioPCMFinal}, nil
	case qol.TypeAudioStream:
		mediaType, _, end, data, err := wire.UnmarshalAudioStream(event.Payload)
		if err != nil {
			return outboundItem{}, err
		}
		encoded, err := ParseEncodedInputMediaType(mediaType)
		if err != nil {
			return outboundItem{}, err
		}
		if len(data) == 0 && !end {
			return outboundItem{}, ErrMalformedMedia
		}
		return outboundItem{data: data, encoded: encoded, span: event.Span, end: end}, nil
	default:
		return outboundItem{}, fmt.Errorf("%w: %s", ErrUnsupportedMedia, event.Type)
	}
}

func pcmKind(format qol.SampleFormat) ProfileKind {
	if format == qol.SampleF32LE {
		return ProfilePCMF32LE
	}
	return ProfilePCMS16LE
}

func sameOutboundFormat(first, item outboundItem) bool {
	if first.encoded.MediaType != "" || item.encoded.MediaType != "" {
		return first.encoded.MediaType != "" && first.encoded.MediaType == item.encoded.MediaType
	}
	return len(item.data) == 0 || SamePCMFormat(first.format, item.format)
}

func PackRTP(frame Frame) *rtp.Packet {
	pt := frame.PayloadType
	if pt == 0 {
		pt = DefaultPayloadType
	}
	ssrc := frame.SSRC
	if ssrc == 0 {
		ssrc = 1
	}
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    pt,
			SequenceNumber: frame.Sequence,
			Timestamp:      frame.Timestamp,
			SSRC:           ssrc,
			Marker:         frame.Marker,
		},
		Payload: frame.Payload,
	}
}

func LittleEndianS16(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(sample))
	}
	return out
}

func wrapProcess(err error, proc *process) error {
	if proc == nil {
		return err
	}
	if detail := proc.Error(); detail != nil && err != nil {
		return fmt.Errorf("%w: %v", err, detail)
	}
	return err
}
