package media_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/cmplx"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/media"
	"github.com/aleksclark/qol/internal/wire"
)

func TestValidateCodecFailsClosed(t *testing.T) {
	err := media.ValidateCodec(media.Codec{MimeType: "audio/pcmu", ClockRate: 8000, Channels: 1})
	if !errors.Is(err, media.ErrUnsupportedCodec) {
		t.Fatalf("got %v, want unsupported codec", err)
	}
	if err := media.ValidateCodec(media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 2}); err != nil {
		t.Fatal(err)
	}
}

func TestQueueOverflowDropsOldest(t *testing.T) {
	q := media.NewQueue[int](2, media.OverflowDropOldest)
	if err := q.Push(1); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(2); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(3); err != nil {
		t.Fatal(err)
	}
	if q.Dropped() != 1 {
		t.Fatalf("dropped %d, want 1", q.Dropped())
	}
	item, ok, err := q.Pop()
	if err != nil || !ok || item != 2 {
		t.Fatalf("got %d ok=%v err=%v", item, ok, err)
	}
}

func TestQueueOverflowFails(t *testing.T) {
	q := media.NewQueue[int](1, media.OverflowFail)
	if err := q.Push(1); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(2); !errors.Is(err, media.ErrOverflow) {
		t.Fatalf("got %v, want overflow", err)
	}
}

func TestPCMProducesOpusTone(t *testing.T) {
	requireFFmpeg(t)
	for _, tc := range []struct {
		rate     int
		channels int
		format   qol.SampleFormat
		freq     float64
	}{
		{22050, 1, qol.SampleS16LE, 440},
		{16000, 1, qol.SampleS16LE, 880},
		{48000, 2, qol.SampleF32LE, 523},
	} {
		t.Run(media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: tc.rate, Channels: tc.channels}.String(), func(t *testing.T) {
			pcm := tonePCM(t, tc.freq, tc.rate, tc.channels, tc.format, 300*time.Millisecond)
			out, err := media.NewOutbound(media.OutboundConfig{
				Codec: media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
				Queue: 16,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := out.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if err := out.WritePCM(qol.Audio{Format: qol.PCMFormat{SampleRate: tc.rate, Channels: tc.channels, Format: tc.format}, Data: pcm}, qol.Span{}, false); err != nil {
				t.Fatal(err)
			}
			if err := out.WritePCM(qol.Audio{Format: qol.PCMFormat{SampleRate: tc.rate, Channels: tc.channels, Format: tc.format}, Data: []byte{0, 0}}, qol.Span{}, true); err != nil && !errors.Is(err, media.ErrClosed) {
				t.Fatal(err)
			}
			frames := collectFrames(t, out.Frames())
			if len(frames) == 0 {
				t.Fatal("no RTP frames before EOS")
			}
			decoded := decodeOpusFrames(t, frames, 16000, 1)
			assertTone(t, decoded, 16000, tc.freq, 250*time.Millisecond)
			if err := out.Close(); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestEncodedStreamProducesOpusBeforeEOS(t *testing.T) {
	requireFFmpeg(t)
	ogg := encodedTone(t, 440, time.Second)
	out, err := media.NewOutbound(media.OutboundConfig{
		Codec: media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
		Queue: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := out.Start(ctx); err != nil {
		t.Fatal(err)
	}
	mid := min(len(ogg)/2, 4096)
	if mid < 2048 && len(ogg) > 2048 {
		mid = 2048
	}
	if err := out.WriteStream(media.MediaTypeOggOpus, ogg[:mid], false); err != nil {
		t.Fatal(err)
	}
	got := firstFrame(t, out.Frames(), 3*time.Second)
	if len(got.Payload) == 0 {
		t.Fatal("expected RTP before EOS")
	}
	if err := out.WriteStream(media.MediaTypeOggOpus, ogg[mid:], true); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestEncodedInputMediaTypesProduceOpusTone(t *testing.T) {
	requireFFmpeg(t)
	for _, tc := range []struct {
		name      string
		mediaType string
		container string
		freq      float64
	}{
		{name: "ogg opus with normalized parameters", mediaType: "Audio/Ogg;Codecs=Opus", container: "ogg", freq: 440},
		{name: "ogg alias", mediaType: "audio/ogg", container: "ogg", freq: 523},
		{name: "mpeg", mediaType: "audio/mpeg", container: "mp3", freq: 660},
		{name: "mp3 alias", mediaType: "audio/mp3", container: "mp3", freq: 880},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := encodedToneContainer(t, tc.container, tc.freq, 700*time.Millisecond)
			out, err := media.NewOutbound(media.OutboundConfig{
				Codec: media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
				Queue: 256,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := out.Start(ctx); err != nil {
				t.Fatal(err)
			}
			for offset, n := 0, 0; offset < len(encoded); offset += n {
				n = []int{137, 911, 43, 2048, 307}[offset%5]
				if n > len(encoded)-offset {
					n = len(encoded) - offset
				}
				if err := out.WriteStream(tc.mediaType, encoded[offset:offset+n], offset+n == len(encoded)); err != nil {
					t.Fatal(err)
				}
			}
			frames := collectFrames(t, out.Frames())
			if err := out.Wait(); err != nil {
				t.Fatal(err)
			}
			if len(frames) == 0 {
				t.Fatal("no RTP frames produced")
			}
			assertTone(t, decodeOpusFrames(t, frames, 16000, 1), 16000, tc.freq, 450*time.Millisecond)
		})
	}
}

func TestUnsupportedAndMalformedEncodedInputsFailBeforeQueueing(t *testing.T) {
	out, err := media.NewOutbound(media.OutboundConfig{
		Codec:    media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
		Queue:    1,
		Overflow: media.OverflowFail,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, mediaType := range []string{"", "application/pdf", "audio/opus", "audio/webm", "audio/ogg; codecs"} {
		err := out.WriteStream(mediaType, []byte("not queued"), false)
		if mediaType == "" || mediaType == "audio/ogg; codecs" {
			if !errors.Is(err, media.ErrMalformedMedia) {
				t.Fatalf("WriteStream(%q) error = %v, want malformed media", mediaType, err)
			}
		} else if !errors.Is(err, media.ErrUnsupportedMedia) {
			t.Fatalf("WriteStream(%q) error = %v, want unsupported media", mediaType, err)
		}
	}
	if err := out.WriteStream(media.MediaTypeOggOpus, []byte("queued"), false); err != nil {
		t.Fatal(err)
	}
}

func TestEncodedMediaTypeChangeFails(t *testing.T) {
	out, err := media.NewOutbound(media.OutboundConfig{Codec: media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1}, Queue: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := out.WriteStream(media.MediaTypeOggOpus, []byte("first"), false); err != nil {
		t.Fatal(err)
	}
	if err := out.WriteStream("audio/mpeg", []byte("second"), true); !errors.Is(err, media.ErrFormatChanged) {
		t.Fatalf("got %v, want format changed", err)
	}
}

func TestMidStreamPCMChangeFails(t *testing.T) {
	out, err := media.NewOutbound(media.OutboundConfig{Codec: media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := wire.MarshalAudio(qol.Audio{Format: qol.PCMFormat{SampleRate: 16000, Channels: 1, Format: qol.SampleS16LE}, Data: bytes.Repeat([]byte{0, 1}, 160)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := wire.MarshalAudio(qol.Audio{Format: qol.PCMFormat{SampleRate: 22050, Channels: 1, Format: qol.SampleS16LE}, Data: bytes.Repeat([]byte{0, 1}, 160)})
	if err != nil {
		t.Fatal(err)
	}
	if err := out.WriteEvent(qol.Event{Type: qol.TypeAudioPCM, Payload: first}); err != nil {
		t.Fatal(err)
	}
	if err := out.WriteEvent(qol.Event{Type: qol.TypeAudioPCM, Payload: second}); !errors.Is(err, media.ErrFormatChanged) {
		t.Fatalf("got %v", err)
	}
}

func TestInboundOpusBecomesConfiguredPCM(t *testing.T) {
	requireFFmpeg(t)
	frames := opusFrames(t, 440, 300*time.Millisecond)
	in, err := media.NewInbound(media.InboundConfig{
		Codec:   media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		Profile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: 16000, Channels: 1},
		Queue:   64,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		if err := in.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.WriteFrame(media.Frame{End: true}); err != nil {
		t.Fatal(err)
	}
	pcm := collectPCM(t, in.Chunks())
	assertTone(t, pcm, 16000, 440, 200*time.Millisecond)
	if err := in.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInboundOpusBecomesOggOpus(t *testing.T) {
	requireFFmpeg(t)
	frames := opusFrames(t, 660, 300*time.Millisecond)
	in, err := media.NewInbound(media.InboundConfig{
		Codec:   media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
		Profile: media.Profile{Kind: media.ProfileOggOpus},
		Queue:   64,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 1)
	go func() {
		var assembled []byte
		for chunk := range in.Chunks() {
			assembled = append(assembled, chunk.Data...)
			if len(assembled) > 64 && len(got) == 0 {
				got <- assembled
			}
			if chunk.End {
				return
			}
		}
	}()
	if err := in.WriteFrame(frames[0]); err != nil {
		t.Fatal(err)
	}
	select {
	case first := <-got:
		if !bytes.Contains(first, []byte("OggS")) {
			t.Fatalf("encoded output is not Ogg: %q", first[:min(16, len(first))])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ogg output did not start before EOS")
	}
	for _, frame := range frames[1:] {
		if err := in.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.WriteFrame(media.Frame{End: true}); err != nil {
		t.Fatal(err)
	}
	if err := in.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInboundOggSpansMatchDecodedDuration(t *testing.T) {
	requireFFmpeg(t)
	const duration = 300 * time.Millisecond
	frames := opusFrames(t, 660, duration)
	in, err := media.NewInbound(media.InboundConfig{
		Codec:   media.Codec{MimeType: media.MimeTypeOpus, ClockRate: media.OpusClockRate, Channels: 1},
		Profile: media.Profile{Kind: media.ProfileOggOpus},
		Queue:   64,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		if err := in.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.WriteFrame(media.Frame{End: true}); err != nil {
		t.Fatal(err)
	}

	var encoded []byte
	var cursor qol.MediaTime
	seenHeader := false
	for chunk := range in.Chunks() {
		if chunk.End {
			if chunk.Span.Start != cursor || chunk.Span.End != cursor {
				t.Fatalf("EOS span = %#v, want cursor %v", chunk.Span, cursor)
			}
			break
		}
		if chunk.Span.Start != cursor || chunk.Span.End < chunk.Span.Start {
			t.Fatalf("non-contiguous span %#v after %v", chunk.Span, cursor)
		}
		if bytes.Contains(chunk.Data, []byte("OpusHead")) {
			seenHeader = true
			if chunk.Span.End != chunk.Span.Start {
				t.Fatalf("header page advanced clock: %#v", chunk.Span)
			}
		}
		cursor = chunk.Span.End
		encoded = append(encoded, chunk.Data...)
	}
	if !seenHeader {
		t.Fatal("missing Opus header page")
	}
	decoded := decodeOggPCM(t, encoded, 16000)
	decodedDuration := time.Duration(len(decoded)/2) * time.Second / 16000
	// FFmpeg's UDP RTP input can discard the final packet while its Ogg output
	// flushes the packet-duration clock. Allow one Opus packet for that
	// transport boundary; granule timing itself remains sample-accurate.
	if difference(cursor.Duration(), decodedDuration) > time.Duration(media.OpusFrameMs)*time.Millisecond {
		t.Fatalf("final span = %v, decoded duration = %v", cursor.Duration(), decodedDuration)
	}
	if err := in.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInboundLossIsObservable(t *testing.T) {
	in, err := media.NewInbound(media.InboundConfig{
		Codec:   media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
		Profile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: 16000, Channels: 1},
		Queue:   8,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := in.WriteFrame(media.Frame{Payload: []byte{1}, Sequence: 1, Timestamp: 0}); err != nil {
		t.Fatal(err)
	}
	if err := in.WriteFrame(media.Frame{Payload: []byte{2}, Sequence: 4, Timestamp: 960}); err != nil {
		t.Fatal(err)
	}
	_ = in.WriteFrame(media.Frame{End: true})
	_ = in.Close()
	if in.Metrics().Discontinuities.Load() == 0 {
		t.Fatal("expected discontinuity counter")
	}
}

func TestOutboundCancellationReapsProcess(t *testing.T) {
	requireFFmpeg(t)
	out, err := media.NewOutbound(media.OutboundConfig{Codec: media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1}, Queue: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := out.Start(ctx); err != nil {
		t.Fatal(err)
	}
	pcm := tonePCM(t, 440, 16000, 1, qol.SampleS16LE, time.Second)
	if err := out.WritePCM(qol.Audio{Format: qol.PCMFormat{SampleRate: 16000, Channels: 1, Format: qol.SampleS16LE}, Data: pcm}, qol.Span{}, false); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := out.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertNoOrphanFFmpeg(t)
}

func TestOutboundMissingExecutableReportsStartupAndCloseError(t *testing.T) {
	out, err := media.NewOutbound(media.OutboundConfig{
		FFmpeg: "definitely-not-a-qol-executable",
		Codec:  media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded with a missing executable")
	}
	if err := out.Close(); err == nil {
		t.Fatal("Close discarded the startup failure")
	}
}

func TestConverterCrashCleansUp(t *testing.T) {
	out, err := media.NewOutbound(media.OutboundConfig{
		FFmpeg: "false",
		Codec:  media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
		Queue:  4,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := out.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_ = out.WritePCM(qol.Audio{Format: qol.PCMFormat{SampleRate: 16000, Channels: 1, Format: qol.SampleS16LE}, Data: bytes.Repeat([]byte{0, 1}, 320)}, qol.Span{}, true)
	select {
	case <-out.Frames():
	case <-time.After(2 * time.Second):
	}
	_ = out.Close()
	assertNoOrphanFFmpeg(t)
}

func TestOutboundQueueOverflowUnderLoad(t *testing.T) {
	out, err := media.NewOutbound(media.OutboundConfig{
		Codec:    media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
		Queue:    2,
		Overflow: media.OverflowFail,
	})
	if err != nil {
		t.Fatal(err)
	}
	pcm := qol.Audio{Format: qol.PCMFormat{SampleRate: 16000, Channels: 1, Format: qol.SampleS16LE}, Data: bytes.Repeat([]byte{0, 1}, 160)}
	if err := out.WritePCM(pcm, qol.Span{}, false); err != nil {
		t.Fatal(err)
	}
	if err := out.WritePCM(pcm, qol.Span{}, false); err != nil {
		t.Fatal(err)
	}
	if err := out.WritePCM(pcm, qol.Span{}, false); !errors.Is(err, media.ErrOverflow) {
		t.Fatalf("got %v, want overflow", err)
	}
	if out.Metrics().Dropped.Load() == 0 {
		t.Fatal("expected dropped counter")
	}
}

func TestMalformedEncodedHeaderFails(t *testing.T) {
	requireFFmpeg(t)
	out, err := media.NewOutbound(media.OutboundConfig{Codec: media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1}, Queue: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := out.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := out.WriteStream(media.MediaTypeOggOpus, []byte("not-an-ogg-header"), true); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case frame, ok := <-out.Frames():
			if !ok {
				goto closed
			}
			if !frame.End && len(frame.Payload) > 0 {
				t.Fatal("malformed ogg must not emit RTP")
			}
		case <-time.After(2 * time.Second):
			goto closed
		}
	}
closed:
	err = out.Close()
	if err == nil {
		t.Fatal("expected converter error for malformed ogg")
	}
}

func TestInboundOggIsIncrementallyDecodableBySTT(t *testing.T) {
	requireFFmpeg(t)
	frames := opusFrames(t, 440, 400*time.Millisecond)
	in, err := media.NewInbound(media.InboundConfig{
		Codec:   media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
		Profile: media.Profile{Kind: media.ProfileOggOpus},
		Queue:   64,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	decoder := media.NewDecoder("ffmpeg")
	stdin, stdout, wait, err := decoder.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decoded := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(stdout)
		decoded <- data
	}()
	gotPage := false
	for _, frame := range frames {
		if err := in.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.WriteFrame(media.Frame{End: true}); err != nil {
		t.Fatal(err)
	}
	for chunk := range in.Chunks() {
		if len(chunk.Data) > 0 {
			if _, err := stdin.Write(chunk.Data); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(chunk.Data, []byte("OggS")) {
				gotPage = true
			}
		}
		if chunk.End {
			break
		}
	}
	_ = stdin.Close()
	_ = wait()
	pcm := <-decoded
	if !gotPage {
		t.Fatal("expected Ogg pages before EOS")
	}
	assertTone(t, pcm, 16000, 440, 200*time.Millisecond)
	if err := in.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func assertNoOrphanFFmpeg(t *testing.T) {
	t.Helper()
	cmd := exec.Command("ps", "-eo", "pid,ppid,stat,args")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	self := strconv.Itoa(os.Getpid())
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "ffmpeg") || strings.Contains(line, "ps ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[1] == self || fields[1] == "1" && strings.Contains(line, "rtp://127.0.0.1") {
			t.Fatalf("orphan ffmpeg still running: %s", line)
		}
	}
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
}

func tonePCM(t *testing.T, freq float64, rate, channels int, format qol.SampleFormat, duration time.Duration) []byte {
	t.Helper()
	samples := int(duration.Seconds() * float64(rate))
	if format == qol.SampleF32LE {
		out := make([]byte, samples*channels*4)
		for i := 0; i < samples; i++ {
			value := float32(math.Sin(2 * math.Pi * freq * float64(i) / float64(rate)))
			for ch := 0; ch < channels; ch++ {
				binary.LittleEndian.PutUint32(out[(i*channels+ch)*4:], math.Float32bits(value))
			}
		}
		return out
	}
	out := make([]byte, samples*channels*2)
	for i := 0; i < samples; i++ {
		value := int16(math.Sin(2*math.Pi*freq*float64(i)/float64(rate)) * 20000)
		for ch := 0; ch < channels; ch++ {
			binary.LittleEndian.PutUint16(out[(i*channels+ch)*2:], uint16(value))
		}
	}
	return out
}

func encodedTone(t *testing.T, freq float64, duration time.Duration) []byte {
	return encodedToneContainer(t, "ogg", freq, duration)
}

func encodedToneContainer(t *testing.T, container string, freq float64, duration time.Duration) []byte {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=" + strconv.Itoa(int(freq)) + ":duration=" + strconv.FormatFloat(duration.Seconds(), 'f', 3, 64)}
	switch container {
	case "ogg":
		args = append(args, "-c:a", "libopus", "-page_duration", "20000", "-f", "ogg")
	case "mp3":
		args = append(args, "-c:a", "libmp3lame", "-f", "mp3")
	default:
		t.Fatalf("unsupported test container %q", container)
	}
	args = append(args, "pipe:1")
	out, err := exec.Command("ffmpeg", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func opusFrames(t *testing.T, freq float64, duration time.Duration) []media.Frame {
	t.Helper()
	out, err := media.NewOutbound(media.OutboundConfig{Codec: media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1}, Queue: 16})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := out.Start(ctx); err != nil {
		t.Fatal(err)
	}
	pcm := tonePCM(t, freq, 48000, 1, qol.SampleS16LE, duration)
	if err := out.WritePCM(qol.Audio{Format: qol.PCMFormat{SampleRate: 48000, Channels: 1, Format: qol.SampleS16LE}, Data: pcm}, qol.Span{}, true); err != nil {
		t.Fatal(err)
	}
	frames := collectFrames(t, out.Frames())
	_ = out.Close()
	return frames
}

func collectFrames(t *testing.T, frames <-chan media.Frame) []media.Frame {
	t.Helper()
	var out []media.Frame
	deadline := time.After(6 * time.Second)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return out
			}
			if frame.End {
				return out
			}
			if len(frame.Payload) > 0 {
				out = append(out, frame)
			}
		case <-deadline:
			if len(out) == 0 {
				t.Fatal("timed out waiting for frames")
			}
			return out
		}
	}
}

func firstFrame(t *testing.T, frames <-chan media.Frame, wait time.Duration) media.Frame {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(wait):
		t.Fatal("timed out waiting for first frame")
		return media.Frame{}
	}
}

func collectPCM(t *testing.T, chunks <-chan media.OutputChunk) []byte {
	t.Helper()
	var out []byte
	var last qol.MediaTime
	deadline := time.After(6 * time.Second)
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return out
			}
			if chunk.End && len(chunk.Data) == 0 {
				return out
			}
			if chunk.Span.End < chunk.Span.Start || (last > 0 && chunk.Span.Start < last) {
				t.Fatalf("non-monotonic span %#v last=%d", chunk.Span, last)
			}
			last = chunk.Span.End
			out = append(out, chunk.Data...)
			if chunk.End {
				return out
			}
		case <-deadline:
			if len(out) == 0 {
				t.Fatal("timed out waiting for pcm")
			}
			return out
		}
	}
}

func decodeOggPCM(t *testing.T, ogg []byte, rate int) []byte {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "ogg", "-i", "pipe:0", "-f", "s16le", "-ar", strconv.Itoa(rate), "-ac", "1", "pipe:1")
	cmd.Stdin = bytes.NewReader(ogg)
	pcm, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return pcm
}

func difference(a, b time.Duration) time.Duration {
	if a < b {
		return b - a
	}
	return a - b
}

func decodeOpusFrames(t *testing.T, frames []media.Frame, rate, channels int) []byte {
	t.Helper()
	in, err := media.NewInbound(media.InboundConfig{
		Codec:   media.Codec{MimeType: media.MimeTypeOpus, ClockRate: 48000, Channels: 1},
		Profile: media.Profile{Kind: media.ProfilePCMS16LE, SampleRate: rate, Channels: channels},
		Queue:   64,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		if err := in.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.WriteFrame(media.Frame{End: true}); err != nil {
		t.Fatal(err)
	}
	pcm := collectPCM(t, in.Chunks())
	_ = in.Close()
	return pcm
}

func assertTone(t *testing.T, pcm []byte, rate int, freq float64, minDur time.Duration) {
	t.Helper()
	if len(pcm) < 64 {
		t.Fatalf("pcm too short: %d", len(pcm))
	}
	samples := make([]float64, len(pcm)/2)
	energy := 0.0
	for i := range samples {
		samples[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		energy += samples[i] * samples[i]
	}
	if energy == 0 {
		t.Fatal("decoded audio is silent")
	}
	got := dominantFreq(samples, rate)
	if math.Abs(got-freq) > freq*0.12 && math.Abs(got-freq) > 40 {
		t.Fatalf("dominant frequency %.1f, want %.1f", got, freq)
	}
	dur := time.Duration(len(samples)) * time.Second / time.Duration(rate)
	if dur < minDur {
		t.Fatalf("duration %s below %s", dur, minDur)
	}
}

func dominantFreq(samples []float64, rate int) float64 {
	n := 1
	for n < len(samples) {
		n <<= 1
	}
	if n > 8192 {
		n = 8192
	}
	if len(samples) > n {
		samples = samples[:n]
	}
	spec := make([]complex128, n)
	for i, sample := range samples {
		spec[i] = complex(sample, 0)
	}
	fft(spec)
	bestBin := 1
	bestMag := 0.0
	for i := 1; i < n/2; i++ {
		mag := cmplx.Abs(spec[i])
		if mag > bestMag {
			bestMag = mag
			bestBin = i
		}
	}
	return float64(bestBin) * float64(rate) / float64(n)
}

func fft(a []complex128) {
	n := len(a)
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for j&bit != 0 {
			j ^= bit
			bit >>= 1
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		angle := -2 * math.Pi / float64(length)
		wlen := cmplx.Rect(1, angle)
		for i := 0; i < n; i += length {
			w := complex(1, 0)
			for k := 0; k < length/2; k++ {
				u := a[i+k]
				v := a[i+k+length/2] * w
				a[i+k] = u + v
				a[i+k+length/2] = u - v
				w *= wlen
			}
		}
	}
}
