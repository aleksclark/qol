package wire

import (
	"errors"

	"github.com/aleksclark/qol/domain"
	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func EventToProto(event domain.Event) (*qolv1.Event, error) {
	if err := event.PCM.Validate(); err != nil {
		return nil, err
	}
	if event.ID == "" || event.SessionID == "" {
		return nil, errors.New("event and session IDs are required")
	}
	return &qolv1.Event{
		EventId:        event.ID,
		SessionId:      event.SessionID,
		Sequence:       event.Sequence,
		Span:           &qolv1.MediaSpan{StartNs: int64(event.Span.Start), EndNs: int64(event.Span.End)},
		ParentEventIds: event.Parents,
		ProducedAt:     timestamppb.New(event.Produced),
		Payload: &qolv1.Event_Pcm{Pcm: &qolv1.PcmChunk{
			SampleRateHz:       event.PCM.SampleRate,
			ChannelCount:       event.PCM.Channels,
			SampleFormat:       qolv1.PcmSampleFormat_PCM_SAMPLE_FORMAT_S16_LE,
			FrameCount:         event.PCM.Frames,
			InterleavedSamples: event.PCM.Data,
		}},
	}, nil
}

func EventFromProto(message *qolv1.Event) (domain.Event, error) {
	if message == nil || message.GetPcm() == nil {
		return domain.Event{}, errors.New("PCM event is required")
	}
	pcm := message.GetPcm()
	if pcm.SampleFormat != qolv1.PcmSampleFormat_PCM_SAMPLE_FORMAT_S16_LE {
		return domain.Event{}, errors.New("unsupported PCM sample format")
	}
	event := domain.Event{
		ID:        message.EventId,
		SessionID: message.SessionId,
		Sequence:  message.Sequence,
		Span:      domain.Span{Start: domain.MediaTime(message.GetSpan().GetStartNs()), End: domain.MediaTime(message.GetSpan().GetEndNs())},
		Parents:   message.ParentEventIds,
		PCM: domain.PCM{
			SampleRate: pcm.SampleRateHz,
			Channels:   pcm.ChannelCount,
			Frames:     pcm.FrameCount,
			Data:       pcm.InterleavedSamples,
		},
	}
	if message.ProducedAt != nil {
		event.Produced = message.ProducedAt.AsTime()
	}
	if event.ID == "" || event.SessionID == "" {
		return domain.Event{}, errors.New("event and session IDs are required")
	}
	if err := event.PCM.Validate(); err != nil {
		return domain.Event{}, err
	}
	return event, nil
}
