package media

import (
	"encoding/binary"
	"io"
	"reflect"
	"testing"
	"time"

	qol "github.com/aleksclark/qol"
)

func TestOggPageReaderClockIgnoresReadBoundaries(t *testing.T) {
	const preSkip = 312
	stream := append(oggTestPage(0, opusHead(preSkip)), oggTestPage(preSkip+960, []byte{1})...)
	stream = append(stream, oggTestPage(preSkip+2400, []byte{2})...)
	stream = append(stream, oggTestPage(preSkip+3576, []byte{3})...)

	var want []qol.Span
	for _, size := range []int{1, 7, 29, 4096} {
		reader := oggPageReader{}
		input := &fixedChunkReader{data: stream, size: size}
		var got []qol.Span
		for {
			page, granule, err := reader.readPage(input)
			if len(page) != 0 {
				got = append(got, reader.span(page, granule))
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read size %d: %v", size, err)
			}
		}
		if want == nil {
			want = got
		} else if !reflect.DeepEqual(got, want) {
			t.Fatalf("read size %d spans = %#v, want %#v", size, got, want)
		}
	}

	if want[0] != (qol.Span{}) {
		t.Fatalf("header span = %#v, want zero duration", want[0])
	}
	if want[1].Start != 0 || want[1].End != qol.MediaTime(20*time.Millisecond) {
		t.Fatalf("first audio span = %#v", want[1])
	}
	if want[2].Start != want[1].End || want[2].End != qol.MediaTime(50*time.Millisecond) {
		t.Fatalf("second audio span = %#v", want[2])
	}
	if want[3].Start != want[2].End || want[3].End != qol.MediaTime(74500*time.Microsecond) {
		t.Fatalf("trimmed final span = %#v", want[3])
	}
}

func TestOggPageReaderRejectsTruncatedPage(t *testing.T) {
	reader := oggPageReader{}
	_, _, err := reader.readPage(&fixedChunkReader{data: oggTestPage(0, []byte{1})[:27], size: 3})
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
}

type fixedChunkReader struct {
	data []byte
	size int
}

func (r *fixedChunkReader) Read(dst []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.size
	if n > len(r.data) {
		n = len(r.data)
	}
	if n > len(dst) {
		n = len(dst)
	}
	copy(dst, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func opusHead(preSkip uint16) []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1
	head[9] = 1
	binary.LittleEndian.PutUint16(head[10:12], preSkip)
	binary.LittleEndian.PutUint32(head[12:16], OpusClockRate)
	return head
}

func oggTestPage(granule uint64, packet []byte) []byte {
	page := make([]byte, 28+len(packet))
	copy(page, "OggS")
	page[4] = 0
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], 1)
	page[26] = 1
	page[27] = byte(len(packet))
	copy(page[28:], packet)
	return page
}
