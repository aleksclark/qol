package media

import (
	"fmt"
	"mime"
	"strings"
)

// EncodedInput describes an encoded stream container that the outbound
// converter can demux incrementally. Demuxer is the corresponding FFmpeg
// input-format name.
type EncodedInput struct {
	MediaType string
	Demuxer   string
}

// encodedInputs is the authoritative encoded-input contract. Do not add an
// entry without a chunked, decoder-backed converter test: declaring a media
// type makes it part of the production wire contract.
var encodedInputs = map[string]EncodedInput{
	"audio/ogg":              {MediaType: "audio/ogg", Demuxer: "ogg"},
	"audio/ogg; codecs=opus": {MediaType: MediaTypeOggOpus, Demuxer: "ogg"},
	"audio/mpeg":             {MediaType: "audio/mpeg", Demuxer: "mp3"},
	"audio/mp3":              {MediaType: "audio/mp3", Demuxer: "mp3"},
}

// ParseEncodedInputMediaType validates a declared stream media type and
// returns its canonical declaration and FFmpeg demuxer. It deliberately does
// not accept raw audio/opus: concatenated payload bytes have no packet framing
// from which a streaming demuxer can safely recover Opus packets.
func ParseEncodedInputMediaType(value string) (EncodedInput, error) {
	if strings.TrimSpace(value) == "" {
		return EncodedInput{}, ErrMalformedMedia
	}

	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return EncodedInput{}, fmt.Errorf("%w: %s", ErrMalformedMedia, value)
	}
	mediaType = strings.ToLower(mediaType)
	for name, param := range params {
		delete(params, name)
		params[strings.ToLower(name)] = strings.ToLower(param)
	}
	canonical := mime.FormatMediaType(mediaType, params)
	input, ok := encodedInputs[canonical]
	if !ok {
		return EncodedInput{}, fmt.Errorf("%w: %s", ErrUnsupportedMedia, value)
	}
	return input, nil
}

// SupportedEncodedInputMediaTypes returns the complete advertised encoded
// input matrix in canonical form.
func SupportedEncodedInputMediaTypes() []string {
	mediaTypes := []string{
		MediaTypeOggOpus,
		"audio/ogg",
		"audio/mpeg",
		"audio/mp3",
	}
	return mediaTypes
}
