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
	done    chan error
	mu      sync.Mutex
	format  qol.PCMFormat
	hasPCM  bool
	closed  bool
}

type outboundItem struct {
	data   []byte
	format qol.PCMFormat
	kind   ProfileKind
	span   qol.Span
	end    bool
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

func (o *Outbound) Metrics() *Metrics { return o.metrics }

func (o *Outbound) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	o.cancel = cancel
	o.done = make(chan error, 1)
	go func() {
		o.done <- o.encode(runCtx)
	}()
	return nil
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
	if mediaType == "" || (len(data) == 0 && !end) {
		return ErrMalformedMedia
	}
	if !supportedEncoded(mediaType) {
		return fmt.Errorf("%w: %s", ErrUnsupportedMedia, mediaType)
	}
	return o.enqueue(outboundItem{data: append([]byte(nil), data...), kind: ProfileOggOpus, end: end})
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
	if o.done == nil {
		return nil
	}
	return <-o.done
}

func (o *Outbound) enqueue(item outboundItem) error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return ErrClosed
	}
	if item.kind != ProfileOggOpus {
		if o.hasPCM && !SamePCMFormat(o.format, item.format) && len(item.data) > 0 {
			o.mu.Unlock()
			return ErrFormatChanged
		}
		if len(item.data) > 0 {
			o.format = item.format
			o.hasPCM = true
		}
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
	if waitErr := proc.Wait(); waitErr != nil && o.metrics.FramesOut.Load() == 0 {
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
		if item.kind != ProfileOggOpus && first.kind != ProfileOggOpus && !SamePCMFormat(first.format, item.format) && len(item.data) > 0 {
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
	if item.kind == ProfileOggOpus {
		args = append(args, "-probesize", "2048", "-analyzeduration", "0", "-f", "ogg")
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
		if !supportedEncoded(mediaType) {
			return outboundItem{}, fmt.Errorf("%w: %s", ErrUnsupportedMedia, mediaType)
		}
		return outboundItem{data: data, kind: ProfileOggOpus, span: event.Span, end: end}, nil
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

func supportedEncoded(mediaType string) bool {
	switch mediaType {
	case MediaTypeOggOpus, "audio/ogg", "audio/opus", "audio/mpeg", "audio/mp3", "audio/webm", "audio/webm;codecs=opus":
		return true
	default:
		return false
	}
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
