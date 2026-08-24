package wire

import (
	"errors"

	qol "github.com/aleksclark/qol"
	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func EventToProto(event qol.Event) (*qolv1.Event, error) {
	if err := ValidateEvent(event); err != nil {
		return nil, err
	}
	return &qolv1.Event{
		EventId:        event.ID,
		SessionId:      event.SessionID,
		Channel:        event.Channel,
		Type:           event.Type,
		Sequence:       event.Seq,
		Span:           &qolv1.MediaSpan{StartNs: int64(event.Span.Start), EndNs: int64(event.Span.End)},
		ParentEventIds: append([]string(nil), event.Parents...),
		ProducedAt:     timestamppb.New(event.ProducedAt),
		Encoding:       event.Encoding,
		Payload:        append([]byte(nil), event.Payload...),
	}, nil
}

func EventFromProto(message *qolv1.Event) (qol.Event, error) {
	if message == nil {
		return qol.Event{}, errors.New("event is required")
	}
	event := qol.Event{
		ID:        message.EventId,
		SessionID: message.SessionId,
		Channel:   message.Channel,
		Type:      message.Type,
		Seq:       message.Sequence,
		Span:      qol.Span{Start: qol.MediaTime(message.GetSpan().GetStartNs()), End: qol.MediaTime(message.GetSpan().GetEndNs())},
		Parents:   append([]string(nil), message.ParentEventIds...),
		Encoding:  message.Encoding,
		Payload:   append([]byte(nil), message.Payload...),
	}
	if message.ProducedAt != nil {
		if err := message.ProducedAt.CheckValid(); err != nil {
			return qol.Event{}, err
		}
		event.ProducedAt = message.ProducedAt.AsTime()
	}
	if err := ValidateEvent(event); err != nil {
		return qol.Event{}, err
	}
	return event, nil
}

func ValidateEvent(event qol.Event) error {
	if event.ID == "" || event.SessionID == "" {
		return errors.New("event and session IDs are required")
	}
	if event.Channel == "" || event.Type == "" || event.Encoding == "" {
		return errors.New("event channel, type, and encoding are required")
	}
	if event.Span.End < event.Span.Start {
		return errors.New("event span end precedes start")
	}
	if len(event.Payload) == 0 {
		return errors.New("event payload is required")
	}
	return nil
}

func MarshalAudio(value qol.Audio) ([]byte, error) {
	format, err := sampleFormatToProto(value.Format.Format)
	if err != nil {
		return nil, err
	}
	if value.Format.SampleRate <= 0 || value.Format.Channels <= 0 || len(value.Data) == 0 {
		return nil, errors.New("audio format and data are required")
	}
	return proto.Marshal(&qolv1.AudioPayload{Format: &qolv1.PcmFormat{SampleRateHz: uint32(value.Format.SampleRate), ChannelCount: uint32(value.Format.Channels), SampleFormat: format}, Data: value.Data})
}

func UnmarshalAudio(payload []byte) (qol.Audio, error) {
	message := new(qolv1.AudioPayload)
	if err := proto.Unmarshal(payload, message); err != nil {
		return qol.Audio{}, err
	}
	if message.Format == nil || len(message.Data) == 0 {
		return qol.Audio{}, errors.New("audio format and data are required")
	}
	format, err := sampleFormatFromProto(message.Format.SampleFormat)
	if err != nil {
		return qol.Audio{}, err
	}
	return qol.Audio{Format: qol.PCMFormat{SampleRate: int(message.Format.SampleRateHz), Channels: int(message.Format.ChannelCount), Format: format}, Data: append([]byte(nil), message.Data...)}, nil
}

func MarshalText(value qol.Text) ([]byte, error) {
	if value.Text == "" || value.Language == "" {
		return nil, errors.New("text and language are required")
	}
	return proto.Marshal(&qolv1.TextPayload{Text: value.Text, Final: value.Final, Language: value.Language, Speaker: value.Speaker, Confidence: value.Confidence})
}

func UnmarshalText(payload []byte) (qol.Text, error) {
	message := new(qolv1.TextPayload)
	if err := proto.Unmarshal(payload, message); err != nil {
		return qol.Text{}, err
	}
	value := qol.Text{Text: message.Text, Final: message.Final, Language: message.Language, Speaker: message.Speaker, Confidence: message.Confidence}
	if value.Text == "" || value.Language == "" {
		return qol.Text{}, errors.New("text and language are required")
	}
	return value, nil
}

func MarshalTranslation(value qol.Translation) ([]byte, error) {
	if value.Text == "" || value.SourceLanguage == "" || value.TargetLanguage == "" {
		return nil, errors.New("translation text and languages are required")
	}
	return proto.Marshal(&qolv1.TranslationPayload{Text: value.Text, SourceLanguage: value.SourceLanguage, TargetLanguage: value.TargetLanguage, Final: value.Final})
}

func UnmarshalTranslation(payload []byte) (qol.Translation, error) {
	message := new(qolv1.TranslationPayload)
	if err := proto.Unmarshal(payload, message); err != nil {
		return qol.Translation{}, err
	}
	value := qol.Translation{Text: message.Text, SourceLanguage: message.SourceLanguage, TargetLanguage: message.TargetLanguage, Final: message.Final}
	if value.Text == "" || value.SourceLanguage == "" || value.TargetLanguage == "" {
		return qol.Translation{}, errors.New("translation text and languages are required")
	}
	return value, nil
}

func MarshalStoredAudio(value qol.StoredAudio) ([]byte, error) {
	if value.MediaType == "" || value.Path == "" {
		return nil, errors.New("stored audio media type and path are required")
	}
	return proto.Marshal(&qolv1.StoredAudioPayload{MediaType: value.MediaType, Path: value.Path, SizeBytes: value.SizeBytes})
}

func UnmarshalStoredAudio(payload []byte) (qol.StoredAudio, error) {
	message := new(qolv1.StoredAudioPayload)
	if err := proto.Unmarshal(payload, message); err != nil {
		return qol.StoredAudio{}, err
	}
	value := qol.StoredAudio{MediaType: message.MediaType, Path: message.Path, SizeBytes: message.SizeBytes}
	if value.MediaType == "" || value.Path == "" {
		return qol.StoredAudio{}, errors.New("stored audio media type and path are required")
	}
	return value, nil
}

func MarshalAudioStream(mediaType, name string, end bool, data []byte) ([]byte, error) {
	if mediaType == "" || len(data) == 0 && !end {
		return nil, errors.New("audio stream media type and data or end are required")
	}
	return proto.Marshal(&qolv1.AudioStreamPayload{MediaType: mediaType, Name: name, EndOfStream: end, Data: data})
}

func UnmarshalAudioStream(payload []byte) (string, string, bool, []byte, error) {
	message := new(qolv1.AudioStreamPayload)
	if err := proto.Unmarshal(payload, message); err != nil {
		return "", "", false, nil, err
	}
	if message.MediaType == "" || len(message.Data) == 0 && !message.EndOfStream {
		return "", "", false, nil, errors.New("audio stream media type and data or end are required")
	}
	return message.MediaType, message.Name, message.EndOfStream, append([]byte(nil), message.Data...), nil
}

func sampleFormatToProto(format qol.SampleFormat) (qolv1.PcmSampleFormat, error) {
	switch format {
	case qol.SampleS16LE:
		return qolv1.PcmSampleFormat_PCM_SAMPLE_FORMAT_S16_LE, nil
	case qol.SampleF32LE:
		return qolv1.PcmSampleFormat_PCM_SAMPLE_FORMAT_F32_LE, nil
	default:
		return qolv1.PcmSampleFormat_PCM_SAMPLE_FORMAT_UNSPECIFIED, errors.New("unsupported PCM sample format")
	}
}

func sampleFormatFromProto(format qolv1.PcmSampleFormat) (qol.SampleFormat, error) {
	switch format {
	case qolv1.PcmSampleFormat_PCM_SAMPLE_FORMAT_S16_LE:
		return qol.SampleS16LE, nil
	case qolv1.PcmSampleFormat_PCM_SAMPLE_FORMAT_F32_LE:
		return qol.SampleF32LE, nil
	default:
		return 0, errors.New("unsupported PCM sample format")
	}
}
