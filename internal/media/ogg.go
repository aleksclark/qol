package media

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"

	qol "github.com/aleksclark/qol"
)

const maxOggPageSize = 27 + 255 + 255*255

// oggPageReader groups arbitrary pipe reads into complete Ogg pages. Ogg page
// granule positions, rather than pipe read boundaries, provide the media clock.
type oggPageReader struct {
	pending  []byte
	preSkip  uint16
	haveHead bool
	cursor   qol.MediaTime
}

func (r *oggPageReader) readPage(src io.Reader) ([]byte, uint64, error) {
	for {
		if len(r.pending) >= 27 {
			if string(r.pending[:4]) != "OggS" {
				return nil, 0, fmt.Errorf("invalid ogg capture pattern")
			}
			segments := int(r.pending[26])
			headerSize := 27 + segments
			if len(r.pending) >= headerSize {
				bodySize := 0
				for _, size := range r.pending[27:headerSize] {
					bodySize += int(size)
				}
				pageSize := headerSize + bodySize
				if pageSize > maxOggPageSize {
					return nil, 0, fmt.Errorf("ogg page exceeds maximum size")
				}
				if len(r.pending) >= pageSize {
					page := append([]byte(nil), r.pending[:pageSize]...)
					r.pending = r.pending[pageSize:]
					return page, binary.LittleEndian.Uint64(page[6:14]), nil
				}
			}
		}

		var buf [4096]byte
		n, err := src.Read(buf[:])
		if n > 0 {
			r.pending = append(r.pending, buf[:n]...)
			if len(r.pending) > maxOggPageSize {
				return nil, 0, fmt.Errorf("ogg page exceeds maximum size")
			}
		}
		if err != nil {
			if err == io.EOF && len(r.pending) != 0 {
				return nil, 0, io.ErrUnexpectedEOF
			}
			return nil, 0, err
		}
	}
}

func (r *oggPageReader) span(page []byte, granule uint64) qol.Span {
	// OpusHead is always in the first packet of an Ogg Opus stream. Its
	// pre-skip is part of the container clock but not decoded playback time.
	if !r.haveHead {
		if offset := indexOpusHead(page); offset >= 0 && len(page) >= offset+12 {
			r.preSkip = binary.LittleEndian.Uint16(page[offset+10 : offset+12])
			r.haveHead = true
		}
	}

	span := qol.Span{Start: r.cursor, End: r.cursor}
	// A granule position of -1 denotes a page with no completed packets.
	// Header pages and incomplete-packet pages therefore do not advance time.
	if granule == ^uint64(0) || !r.haveHead || granule <= uint64(r.preSkip) {
		return span
	}
	end := qol.MediaTime(time.Duration(granule-uint64(r.preSkip)) * time.Second / OpusClockRate)
	if end > r.cursor {
		span.End = end
		r.cursor = end
	}
	return span
}

func indexOpusHead(page []byte) int {
	for offset := 0; ; {
		i := offset
		for i < len(page) && page[i] != 'O' {
			i++
		}
		if i == len(page) || len(page)-i < len("OpusHead") {
			return -1
		}
		if string(page[i:i+len("OpusHead")]) == "OpusHead" {
			return i
		}
		offset = i + 1
	}
}
